package petkit

import (
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/pcm"
	"github.com/pion/rtp"
)

// Talkback (browser mic -> camera speaker):
//
//	WebRTC PCMA (G.711 A-law, 8 kHz) -> PCM16 -> 2x upsample -> 16 kHz mono
//	-> AAC-LC encode -> mbuffer_write "auido-out" ring -> media daemon plays it.
//
// The media daemon's speaker wants 16 kHz mono ADTS-AAC (see mbuffer/media RE).
const talkbackSampleRate = 16000

// AddTrack is called by go2rtc when a consumer (e.g. a browser mic) provides
// audio for our sendonly backchannel media. It starts a speak session on the
// camera and wires the incoming RTP audio through the transcode-and-write path.
func (p *Producer) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	if p.sender == nil {
		_ = speakerEnable(true)
		if err := speakStart(); err != nil {
			return err
		}
		p.enc = newAACEncoder(talkbackSampleRate, 1)
		p.pcmBuf = p.pcmBuf[:0]
		p.sender = core.NewSender(media, track.Codec)
		p.sender.Handler = p.handleTalkbackRTP
	}
	p.sender.HandleRTP(track)
	return nil
}

// handleTalkbackRTP converts one incoming PCMA packet to 16 kHz PCM, then emits
// AAC-LC frames into the camera's audio-out ring whenever a full 1024-sample
// frame has accumulated.
func (p *Producer) handleTalkbackRTP(pkt *rtp.Packet) {
	// Decode G.711 A-law -> PCM16 (8 kHz) and 2x linear-upsample to 16 kHz.
	for _, alaw := range pkt.Payload {
		s := pcm.PCMAtoPCM(alaw)
		p.pcmBuf = append(p.pcmBuf, int16((int(p.prevSample)+int(s))/2), s)
		p.prevSample = s
	}

	for len(p.pcmBuf) >= aacFrameSamples {
		adts := p.enc.EncodeFrame(p.pcmBuf[:aacFrameSamples])
		if len(adts) > 0 {
			_ = p.mb.WriteAudioFrame(adts, uint64(time.Now().UnixNano()/1000), p.aacIdx)
			p.aacIdx++
		}
		p.pcmBuf = p.pcmBuf[aacFrameSamples:]
	}
}
