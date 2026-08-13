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

// talkbackSession holds one talker's encoder state: the PCM accumulator, the
// upsampler's last sample, and its own AAC encoder instance. Kept per-track
// (rather than shared on the Producer) because AddTrack is called once per
// consumer that opens the backchannel — with a single shared pcmBuf/enc, two
// people talking at once (or a second browser tab left connected) spliced
// their RTP packets into the same accumulator in arrival order, corrupting
// both streams into garbled, dropped-sounding audio. Each session's frames
// still land on the one physical speaker via WriteAudioFrame, which the
// mbuffer's shared mutex already serialises, so concurrent talkers now just
// alternate frames cleanly instead of interleaving mid-sample.
type talkbackSession struct {
	enc        *aacEncoder
	pcmBuf     []int16 // 16 kHz mono accumulator until a 1024-sample AAC frame
	prevSample int16   // last 8 kHz sample, for 2x upsampling
}

// AddTrack is called by go2rtc when a consumer (e.g. a browser mic) provides
// audio for our sendonly backchannel media. It starts a speak session on the
// camera (once) and wires the incoming RTP audio through its own
// transcode-and-write path.
func (p *Producer) AddTrack(media *core.Media, _ *core.Codec, track *core.Receiver) error {
	p.talkbackMu.Lock()
	if p.talkbackSessions == nil {
		p.talkbackSessions = make(map[*core.Receiver]*talkbackSession)
	}
	first := len(p.talkbackSessions) == 0
	sess := &talkbackSession{enc: newAACEncoder(talkbackSampleRate, 1)}
	p.talkbackSessions[track] = sess
	p.talkbackMu.Unlock()

	if first {
		// Open a speak session (best-effort) so the media daemon runs its
		// "auido-out" reader.
		startTalkback()
	}

	sender := core.NewSender(media, track.Codec)
	sender.Handler = func(pkt *rtp.Packet) { p.handleTalkbackRTP(sess, pkt) }
	// Register on the connection so /api/streams shows each talker's
	// packets/drops (drops > 0 = encoder slower than real time).
	p.Senders = append(p.Senders, sender)
	sender.HandleRTP(track)
	return nil
}

// handleTalkbackRTP converts one incoming PCMA packet to 16 kHz PCM, then emits
// AAC-LC frames into the camera's audio-out ring whenever a full 1024-sample
// frame has accumulated.
func (p *Producer) handleTalkbackRTP(sess *talkbackSession, pkt *rtp.Packet) {
	// Decode G.711 A-law -> PCM16 (8 kHz) and 2x linear-upsample to 16 kHz.
	for _, alaw := range pkt.Payload {
		s := pcm.PCMAtoPCM(alaw)
		sess.pcmBuf = append(sess.pcmBuf, int16((int(sess.prevSample)+int(s))/2), s)
		sess.prevSample = s
	}

	for len(sess.pcmBuf) >= aacFrameSamples {
		adts := sess.enc.EncodeFrame(sess.pcmBuf[:aacFrameSamples])
		if len(adts) > 0 {
			if err := p.mb.WriteAudioFrame(aacPayload(adts), uint64(time.Now().UnixNano()/1000), 0); err != nil {
				// Not best-effort-silent anymore: a dropped ring write is a
				// dropped ~64ms chunk of speech. Surfaced via ?debug=1 so a
				// "swallowed" word can actually be correlated to a cause
				// (ring mutex contention with the video writer, most likely).
				p.cfg.dbg("talkback: ring write failed, frame dropped: %v", err)
			}
		}
		sess.pcmBuf = sess.pcmBuf[aacFrameSamples:]
	}
}
