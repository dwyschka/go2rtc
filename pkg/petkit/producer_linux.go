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

	mb, err := OpenMBuffer()
	if err != nil {
		return nil, err
	}

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

	// Ask the camera pipeline to start producing the requested plane/audio.
	// Best-effort: if the camera is already producing this plane (e.g. another
	// client is active) frames flow without it, so a dispatch failure must not
	// block streaming. Any error is surfaced only if probing then finds no
	// frames.
	dispatchErr := sendMediaType(dispatchDstModule, dispatchMsgID, dispatchSrcModule, cfg.mediaType)

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

	if err = prod.probe(); err != nil {
		_ = prod.Stop()
		if dispatchErr != nil {
			return nil, fmt.Errorf("%w (dispatch to %s also failed: %v)",
				err, "/msg_dispatch_1", dispatchErr)
		}
		return nil, err
	}

	return prod, nil
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

	for videoCodec == nil || (needAudio && audioCodec == nil) {
		if time.Now().After(deadline) {
			break
		}

		f, err := p.reader.ReadFrame(500)
		if err != nil {
			if errors.Is(err, errTimeout) || errors.Is(err, errFrameSize) {
				continue
			}
			return err
		}

		if f.Flags&mediaAudio != 0 {
			// Audio frame.
			if needAudio && audioCodec == nil && len(f.Data) >= aac.ADTSHeaderSize {
				if c := aac.ADTSToCodec(f.Data); c != nil {
					c.PayloadType = core.PayloadTypeRAW
					audioCodec = c
				}
			}
			continue
		}

		// Video frame — build the codec from any AU that carries an SPS. We do
		// not rely on the frame header's keyframe flag (its offset is not
		// verified across firmware variants); the AU bytes are authoritative.
		if videoCodec == nil {
			if avcc := annexb.EncodeToAVCC(f.Data); containsSPS(avcc) {
				videoCodec = h264.AVCCToCodec(avcc)
			}
		}
	}

	if videoCodec == nil {
		return errors.New("petkit: no video keyframe seen while probing " +
			"(camera not producing this plane, or frame layout differs from the spec)")
	}

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

	// Talkback backchannel: advertise that we accept G.711 A-law audio to play
	// on the camera speaker. Browsers negotiate PCMA directly over WebRTC, which
	// avoids needing an Opus decoder on the device.
	p.Medias = append(p.Medias, &core.Media{
		Kind:      core.KindAudio,
		Direction: core.DirectionSendonly,
		Codecs: []*core.Codec{
			{Name: core.CodecPCMA, ClockRate: 8000, PayloadType: 8},
		},
	})

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

	// Map the receivers requested by downstream consumers to video/audio.
	for _, recv := range p.Receivers {
		switch recv.Codec.Kind() {
		case core.KindVideo:
			p.video = recv
		case core.KindAudio:
			p.audio = recv
		}
	}

	for {
		f, err := p.reader.ReadFrame(readTimeoutMs)
		if err != nil {
			if errors.Is(err, errTimeout) || errors.Is(err, errFrameSize) {
				// A desync/timeout means we may have skipped frames — the next
				// video output must wait for a keyframe.
				p.needKey = true
				continue
			}
			return err
		}

		// If the ring lapped or underran, drop video until the next keyframe so
		// the decoder never receives reference frames whose base is missing.
		if p.reader.TakeLost() {
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

// Stop releases the reader slot and unmaps the shared memory.
func (p *Producer) Stop() error {
	err := p.Connection.Stop()
	if p.sender != nil {
		p.sender.Close()
		_ = speakStop()
	}
	if p.reader != nil {
		p.reader.Release()
	}
	return err
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
