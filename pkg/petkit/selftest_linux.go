package petkit

import (
	"fmt"
	"math"
	"os"
	"time"
)

// TalkbackDiag probes the pieces the talkback path needs and returns a
// human-readable report: privilege level, shared-memory access, and whether
// each dispatch mqueue can be written. Use it to pinpoint why no sound plays.
func TalkbackDiag() []string {
	var r []string
	r = append(r, fmt.Sprintf("euid=%d (root=%v)", os.Geteuid(), os.Geteuid() == 0))

	mb, err := OpenMBuffer()
	if err != nil {
		return append(r, "shm /media_buffer_frame_buf: FAIL — "+err.Error())
	}
	defer mb.Close()
	r = append(r, fmt.Sprintf("shm /media_buffer_frame_buf: OK (ring %d bytes)", mb.ringSize))

	diag := func(label string, dst, msg uint16, payload []byte) {
		if err := dispatchSend(dst, msg, payload); err != nil {
			r = append(r, fmt.Sprintf("%s: FAIL — %v", label, err))
		} else {
			r = append(r, label+": OK")
		}
	}
	diag("speaker_enable /msg_dispatch_2 msg18", 2, 18, []byte{1, 0, 0, 0})
	diag("speak_start   /msg_dispatch_2 msg5", 2, 5, nil)
	diag("ping          /msg_dispatch_10 msg1", 10, 1, nil)
	diag("ping          /msg_dispatch_13 msg1", 13, 1, []byte{0, 0, 0, 0})
	return r
}

// SelfTestTone plays a sine tone on the camera speaker for the given duration
// via the talkback path (speak_start + mbuffer write + media daemon decode).
// It exercises exactly the new on-device code without a browser, WebRTC, mic or
// HTTPS — the fastest way to confirm the speaker path works.
//
// freqHz is the tone frequency; 0 uses 1000 Hz.
func SelfTestTone(dur time.Duration, freqHz float64) error {
	if freqHz <= 0 {
		freqHz = 1000
	}

	mb, err := OpenMBuffer()
	if err != nil {
		return err
	}
	defer mb.Close()

	// Open a speak session (best-effort) so the media daemon runs its audio-out
	// reader, then write frames; EOS marker at the end.
	startTalkback()
	defer mb.WriteAudioFrame(nil, uint64(time.Now().UnixNano()/1000), 0)

	enc := newAACEncoder(talkbackSampleRate, 1)

	const frameDur = time.Duration(aacFrameSamples) * time.Second / talkbackSampleRate // 64 ms
	frames := int(dur / frameDur)
	phase := 0.0
	step := 2 * math.Pi * freqHz / talkbackSampleRate

	pcm := make([]int16, aacFrameSamples)
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	var idx uint32
	for i := 0; i < frames; i++ {
		for j := range pcm {
			pcm[j] = int16(12000 * math.Sin(phase))
			phase += step
		}
		if adts := enc.EncodeFrame(pcm); len(adts) > 0 {
			if err = mb.WriteAudioFrame(adts, uint64(time.Now().UnixNano()/1000), idx); err != nil {
				return err
			}
			idx++
		}
		<-ticker.C
	}
	return nil
}
