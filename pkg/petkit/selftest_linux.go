package petkit

import (
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
)

// SelfTestFile plays an existing ADTS-AAC file through the talkback ring path:
// it parses the file's AAC frames and writes each into the ring, paced in real
// time. This uses device-native audio so it isolates the ring/reader path from
// our own encoder.
func SelfTestFile(path string) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("petkit: recovered in SelfTestFile: %v", e)
		}
	}()

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !aac.IsADTS(data) {
		return errors.New("petkit: not an ADTS-AAC file: " + path)
	}
	rate := 16000
	if c := aac.ADTSToCodec(data); c != nil {
		rate = int(c.ClockRate)
	}
	frameDur := time.Duration(aacFrameSamples) * time.Second / time.Duration(rate)
	fmt.Printf("playing %s: %d bytes, %d Hz, frame=%v\n", path, len(data), rate, frameDur)

	mb, err := OpenMBuffer()
	if err != nil {
		return err
	}
	defer mb.Close()

	startTalkback()
	defer stopTalkback()
	defer mb.WriteAudioFrame(nil, uint64(time.Now().UnixNano()/1000), 0)

	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	var idx uint32
	var ptsUs uint64
	for i := 0; i+aac.ADTSHeaderSize <= len(data); {
		if !aac.IsADTS(data[i:]) {
			break
		}
		size := int(aac.ReadADTSSize(data[i:]))
		if size < aac.ADTSHeaderSize || i+size > len(data) {
			break
		}
		if err = mb.WriteAudioFrame(aacPayload(data[i:i+size]), ptsUs, idx); err != nil {
			return err
		}
		i += size
		idx++
		ptsUs += uint64(frameDur.Microseconds())
		<-ticker.C
	}
	fmt.Printf("wrote %d AAC frames\n", idx)
	return nil
}

// TalkbackDiag probes the pieces the talkback path needs, printing each step
// live so a crash's last line pinpoints the failing operation. Each step is
// wrapped in recover() so one failure doesn't abort the rest.
func TalkbackDiag() {
	log := func(format string, a ...any) { fmt.Printf("  "+format+"\n", a...) }

	step := func(name string, fn func()) {
		defer func() {
			if e := recover(); e != nil {
				log("%s: PANIC %v", name, e)
			}
		}()
		fn()
	}

	log("euid=%d (root=%v)", os.Geteuid(), os.Geteuid() == 0)

	mb, err := OpenMBuffer()
	if err != nil {
		log("shm /media_buffer_frame_buf: FAIL — %v", err)
		return
	}
	defer mb.Close()
	log("shm /media_buffer_frame_buf: OK (ring %d bytes)", mb.ringSize)

	step("list /dev/mqueue", func() {
		entries, err := os.ReadDir("/dev/mqueue")
		if err != nil {
			log("/dev/mqueue: not listable — %v", err)
			return
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		log("/dev/mqueue: %v", names)
	})

	step("probe queues", func() {
		for _, name := range []string{"/msg_dispatch_1", "/msg_dispatch_2"} {
			if errno := mqProbe(name); errno == 0 {
				log("%s: OPEN OK", name)
			} else {
				log("%s: errno=%d (%v)", name, int(errno), errno)
			}
		}
	})

	step("readers before", func() { log("readers before:      %v", mb.ActiveReaders()) })
	step("startTalkback", func() { startTalkback() })
	time.Sleep(300 * time.Millisecond)
	step("readers after", func() { log("readers after speak: %v", mb.ActiveReaders()) })
}

// SelfTestTone plays a sine tone on the camera speaker for the given duration
// via the talkback path (speak_start + mbuffer write + media daemon decode).
// It exercises exactly the new on-device code without a browser, WebRTC, mic or
// HTTPS — the fastest way to confirm the speaker path works.
//
// freqHz is the tone frequency; 0 uses 1000 Hz.
func SelfTestTone(dur time.Duration, freqHz float64) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("petkit: recovered in SelfTestTone: %v", e)
		}
	}()
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
	defer stopTalkback()
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
			if err = mb.WriteAudioFrame(aacPayload(adts), uint64(time.Now().UnixNano()/1000), idx); err != nil {
				return err
			}
			idx++
		}
		<-ticker.C
	}
	return nil
}
