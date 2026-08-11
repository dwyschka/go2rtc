package petkit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
)

// containsNALU reports whether an AVCC-formatted buffer holds a NALU of any of
// the given types. It bounds-checks every step so malformed device data can't
// panic.
func containsNALU(avcc []byte, types ...byte) bool {
	for len(avcc) >= 5 {
		size := int(binary.BigEndian.Uint32(avcc))
		if size <= 0 || 4+size > len(avcc) {
			return false
		}
		t := avcc[4] & 0x1F
		for _, want := range types {
			if t == want {
				return true
			}
		}
		avcc = avcc[4+size:]
	}
	return false
}

func containsSPS(avcc []byte) bool {
	return containsNALU(avcc, h264.NALUTypeSPS)
}

// containsKeyframe reports whether the AU carries an IDR slice (or SPS, which
// always precedes one in this stream) — i.e. a decodable resync point.
func containsKeyframe(avcc []byte) bool {
	return containsNALU(avcc, h264.NALUTypeIFrame, h264.NALUTypeSPS)
}

const readerName = "ts-server"

// probeTimeout bounds how long Dial waits to see the codecs it needs before
// giving up.
const probeTimeout = 8 * time.Second

// readTimeoutMs is the per-frame wait in the running loop.
const readTimeoutMs = 5000

// Producer reads the camera's shared-memory ring and feeds H.264 + AAC into
// go2rtc. It replaces the device's tserver process.
type Producer struct {
	core.Connection

	mb     *MBuffer
	reader *Reader
	cfg    config

	video *core.Receiver
	audio *core.Receiver
	jpeg  *core.Receiver // native hardware JPEG snapshots (frame.jpeg/stream.mjpeg)

	jpegStop chan struct{} // closed by Stop to end the JPEG pump goroutine

	needKey bool // after a frame loss, drop video until the next keyframe

	// backchannel (talkback: browser mic -> camera speaker)
	sender     *core.Sender
	enc        *aacEncoder
	pcmBuf     []int16 // 16 kHz mono accumulator until a 1024-sample AAC frame
	prevSample int16   // last 8 kHz sample, for 2x upsampling
	aacIdx     uint32  // frame_index for written audio frames
}

// Dial opens the shared-memory ring, registers the "ts-server" reader, tells
// the camera which plane/audio to produce, probes the codecs, and returns a
// ready producer. Only works on the device (Linux).
func Dial(source string) (core.Producer, error) {
	cfg, err := parseSource(source)
	if err != nil {
		return nil, err
	}

	cfg.dbg("dial %q -> plane=%s audio=%v talkback=%v mediaType=0x%02x layout=%s",
		source, cfg.plane, cfg.audio, cfg.talkback, cfg.mediaType, cfg.layout.name)

	mb, err := OpenMBuffer(cfg.layout)
	if err != nil {
		cfg.dbg("OpenMBuffer failed: %v", err)
		return nil, err
	}
	cfg.dbg("mapped %s ring=%d bytes hdr=%#x offsets{num=%#x size=%#x type=%#x flags=%#x sps=%#x}",
		shmPath, mb.ringSize, cfg.layout.hdrSize, cfg.layout.offNum, cfg.layout.offSize,
		cfg.layout.offType, cfg.layout.offFlags, cfg.layout.offSPS)

	reader, err := mb.CreateReader(readerName, true)
	if err != nil {
		_ = mb.Close()
		return nil, err
	}
	if err = reader.SetFilter(uint16(cfg.mediaType)); err != nil {
		reader.Release()
		_ = mb.Close()
		return nil, err
	}

	cfg.dbg("ring writeNum=%d readers=%v", mb.WriteNum(), mb.ActiveReaders())

	prod := &Producer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "petkit",
			Protocol:   "shm",
			RemoteAddr: shmPath,
			Source:     source,
			Transport:  mb,
		},
		mb:     mb,
		reader: reader,
		cfg:    cfg,
	}

	// Force an immediate keyframe (request_IDR, msg 1) so probing — and any
	// joining client — gets an SPS+IDR at once instead of waiting out the
	// encoder's natural GOP, the common cause of a connect-time freeze. This is
	// the dispatch the driver historically sent as the misnamed "start plane";
	// the camera produces the plane regardless, so it only ever forced a keyframe.
	prod.requestIDR()

	if err = prod.probe(); err != nil {
		_ = prod.Stop()
		return nil, err
	}

	return prod, nil
}

// requestIDR asks the hardware encoder to emit an immediate keyframe via the
// media daemon's request_IDR dispatch (msg 1). The payload is the plane bitmask
// (cfg.mediaType: bit2=main/chn0, bit3=sub/chn1) — the handler rejects an empty
// one. Best-effort: a wrong msg_id just means we fall back to the natural GOP.
func (p *Producer) requestIDR() {
	if !p.cfg.forceIDR {
		return
	}
	var payload [4]byte
	putU32(payload[:], 0, p.cfg.mediaType)
	if err := dispatchSendFrom(dispatchIDRModule, msgRequestIDR(), dispatchSrcModule, payload[:]); err != nil {
		p.cfg.dbg("request_IDR failed (best-effort): %v", err)
	} else {
		p.cfg.dbg("request_IDR sent (mediaType=0x%02x)", p.cfg.mediaType)
	}
}

// probe reads frames until it has built the video codec (from the first frame
// carrying an SPS) and, if audio was requested, the audio codec (from the first
// ADTS frame).
func (p *Producer) probe() (err error) {
	// Frame payloads come straight from device shared memory; malformed or
	// unexpected data must never crash the process.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("petkit: recovered while probing: %v", r)
		}
	}()

	deadline := time.Now().Add(probeTimeout)

	var videoCodec, audioCodec *core.Codec
	needAudio := p.cfg.audio

	// Probe-time frame census, surfaced on failure so the operator can tell
	// "camera producing nothing" (all zero) from "wrong layout" (frames arrive
	// but never classify as SPS-carrying video).
	var nFrames, nAudio, nVideo, nSPS int

	for videoCodec == nil || (needAudio && audioCodec == nil) {
		if time.Now().After(deadline) {
			break
		}

		f, err := p.reader.ReadFrame(500)
		if err != nil {
			if errors.Is(err, errTimeout) || errors.Is(err, errFrameSize) {
				p.cfg.dbg("probe read: %v (writeNum=%d)", err, p.mb.WriteNum())
				continue
			}
			return err
		}

		nFrames++
		p.cfg.dbg("frame num=%d flags=0x%04x type=%d size=%d isAudio=%v",
			f.Num, f.Flags, f.Type, len(f.Data), f.Flags&mediaAudio != 0)

		if f.Flags&mediaAudio != 0 {
			// Audio frame.
			nAudio++
			if needAudio && audioCodec == nil && len(f.Data) >= aac.ADTSHeaderSize {
				if c := aac.ADTSToCodec(f.Data); c != nil {
					c.PayloadType = core.PayloadTypeRAW
					audioCodec = c
					p.cfg.dbg("audio codec: %s %dHz", c.Name, c.ClockRate)
				}
			}
			continue
		}

		// Video frame — build the codec from any AU that carries an SPS. We do
		// not rely on the frame header's keyframe flag (its offset is not
		// verified across firmware variants); the AU bytes are authoritative.
		nVideo++
		if videoCodec == nil {
			avcc := annexb.EncodeToAVCC(f.Data)
			if containsSPS(avcc) {
				nSPS++
				videoCodec = h264.AVCCToCodec(avcc)
				p.cfg.dbg("video codec built from SPS at num=%d", f.Num)
			}
		}
	}

	if videoCodec == nil {
		p.cfg.dbg("probe FAILED: frames=%d audio=%d video=%d sps=%d writeNum=%d readers=%v",
			nFrames, nAudio, nVideo, nSPS, p.mb.WriteNum(), p.mb.ActiveReaders())
		return errors.New("petkit: no video keyframe seen while probing " +
			"(camera not producing this plane, or frame layout differs from the spec)")
	}
	p.cfg.dbg("probe OK: frames=%d audio=%d video=%d", nFrames, nAudio, nVideo)

	p.Medias = append(p.Medias, &core.Media{
		Kind:      core.KindVideo,
		Direction: core.DirectionRecvonly,
		Codecs:    []*core.Codec{videoCodec},
	})
	if audioCodec != nil {
		p.Medias = append(p.Medias, &core.Media{
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{audioCodec},
		})
	}

	// Native JPEG snapshots via the device's hardware encoder (see
	// snapshot_linux.go). Advertising a JPEG video track lets go2rtc's
	// frame.jpeg / stream.mjpeg serve it directly — the keyframe consumer
	// prefers JPEG over H.264, so no ffmpeg transcode is needed. The HW encoder
	// only runs while a JPEG consumer is attached, so this is free otherwise.
	if p.cfg.snapshot {
		p.Medias = append(p.Medias, &core.Media{
			Kind:      core.KindVideo,
			Direction: core.DirectionRecvonly,
			Codecs: []*core.Codec{
				{Name: core.CodecJPEG, ClockRate: 90000},
			},
		})
	}

	// Talkback backchannel: advertise that we accept G.711 A-law audio to play
	// on the camera speaker. Browsers negotiate PCMA directly over WebRTC, which
	// avoids needing an Opus decoder on the device. Skipped when the device has
	// no speaker (e.g. the mic-only Ingenic-T7) so no dead talk button appears.
	if p.cfg.talkback {
		p.Medias = append(p.Medias, &core.Media{
			Kind:      core.KindAudio,
			Direction: core.DirectionSendonly,
			Codecs: []*core.Codec{
				{Name: core.CodecPCMA, ClockRate: 8000, PayloadType: 8},
			},
		})
	}

	return nil
}

// Start pumps frames from the ring into the attached receivers until the ring
// closes or a fatal error occurs.
func (p *Producer) Start() (err error) {
	// A malformed frame must end this producer cleanly, not crash go2rtc.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("petkit: recovered in stream loop: %v", r)
		}
	}()

	// Map the receivers requested by downstream consumers. JPEG and H.264 are
	// both KindVideo, so switch on the codec name, not the kind.
	for _, recv := range p.Receivers {
		switch {
		case recv.Codec.Name == core.CodecJPEG:
			p.jpeg = recv
		case recv.Codec.Kind() == core.KindVideo:
			p.video = recv
		case recv.Codec.Kind() == core.KindAudio:
			p.audio = recv
		}
	}

	// Native JPEG snapshots are produced out-of-band from the ring (the device's
	// hardware encoder writes them to a file). Drive that separately.
	if p.jpeg != nil {
		p.jpegStop = make(chan struct{})
		if p.video == nil && p.audio == nil {
			// Snapshot-only consumer (e.g. frame.jpeg): don't spin the ring pump
			// at all — just serve JPEG until Stop unblocks us.
			p.pumpJPEG()
			return nil
		}
		go p.pumpJPEG()
	}

	for {
		f, err := p.reader.ReadFrame(readTimeoutMs)
		if err != nil {
			if errors.Is(err, errTimeout) || errors.Is(err, errFrameSize) {
				// A desync/timeout means we may have skipped frames — the next
				// video output must wait for a keyframe. Ask the encoder for one
				// now (once per gap) so the freeze is as short as possible.
				if !p.needKey {
					p.requestIDR()
				}
				p.needKey = true
				continue
			}
			if errors.Is(err, errClosed) {
				return nil // deliberate Stop unmapped the ring — clean exit
			}
			return err
		}

		// If the ring lapped or underran, drop video until the next keyframe so
		// the decoder never receives reference frames whose base is missing, and
		// ask the encoder to emit one now (once per gap).
		if p.reader.TakeLost() {
			if !p.needKey {
				p.requestIDR()
			}
			p.needKey = true
		}

		p.Recv += len(f.Data)

		if f.Flags&mediaAudio != 0 {
			p.writeAudio(f)
		} else {
			p.writeVideo(f)
		}
	}
}

func (p *Producer) writeVideo(f *Frame) {
	if p.video == nil {
		return
	}
	avcc := annexb.EncodeToAVCC(f.Data)
	if len(avcc) < 5 { // need at least one 4-byte length + NAL header byte
		return
	}
	// After a loss, wait for a keyframe before resuming so the decoder can
	// recover cleanly instead of freezing on undecodable P-frames.
	if p.needKey {
		if !containsKeyframe(avcc) {
			return
		}
		p.needKey = false
	}
	p.video.WriteRTP(&rtp.Packet{
		Header:  rtp.Header{Timestamp: nowRTP(p.video.Codec.ClockRate)},
		Payload: avcc,
	})
}

func (p *Producer) writeAudio(f *Frame) {
	if p.audio == nil {
		return
	}
	ts := nowRTP(p.audio.Codec.ClockRate)

	// A frame may hold one or more concatenated ADTS frames; emit each raw AU.
	data := f.Data
	for len(data) >= aac.ADTSHeaderSize && aac.IsADTS(data) {
		size := int(aac.ReadADTSSize(data))
		if size <= 0 || size > len(data) {
			break
		}
		hdrLen := aac.ADTSHeaderLen(data)
		if hdrLen >= size {
			break
		}
		p.audio.WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Timestamp: ts},
			Payload: data[hdrLen:size],
		})
		data = data[size:]
	}
}

// Stop releases the reader slot and unmaps the shared memory. The petkit
// teardown must run BEFORE Connection.Stop: that closes the Transport (our
// MBuffer), and the EOS frame + reader-slot release need the live mapping.
func (p *Producer) Stop() error {
	if p.jpegStop != nil {
		close(p.jpegStop)
		p.jpegStop = nil
	}
	if p.sender != nil {
		p.sender.Close()
		// End-of-stream marker + tell module 1 to stop its speaker reader.
		_ = p.mb.WriteAudioFrame(nil, uint64(time.Now().UnixNano()/1000), 0)
		stopTalkback()
	}
	if p.reader != nil {
		p.reader.Release()
	}
	return p.Connection.Stop()
}

// nowRTP returns a monotonic RTP timestamp for the given clock rate, derived
// from the wall clock at call time (like core.Now90000, but for any rate).
//
// We deliberately do NOT use the device's per-frame PTS: its header offset and
// unit could not be verified against the ARM firmware, and a wrong or jumpy
// timestamp permanently freezes WebRTC. Arrival-clock timestamps are monotonic
// and correctly paced for live view, and audio + video share this one wall
// clock so they stay in sync.
func nowRTP(clockRate uint32) uint32 {
	return uint32(time.Duration(time.Now().UnixNano()) * time.Duration(clockRate) / time.Second)
}
