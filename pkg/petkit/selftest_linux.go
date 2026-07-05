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

	// List the POSIX mqueue filesystem, if mounted, so we can see the queues
	// and their permissions.
	if entries, err := os.ReadDir("/dev/mqueue"); err != nil {
		r = append(r, "/dev/mqueue: not listable — "+err.Error())
	} else {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		r = append(r, fmt.Sprintf("/dev/mqueue: %v", names))
	}

	for _, name := range []string{"/msg_dispatch_2", "/msg_dispatch_10", "/msg_dispatch_13"} {
		errno := mqProbe(name)
		if errno == 0 {
			r = append(r, name+": OPEN OK")
		} else {
			r = append(r, fmt.Sprintf("%s: errno=%d (%v)", name, int(errno), errno))
		}
	}
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
