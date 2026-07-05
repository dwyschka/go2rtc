package petkit

import (
	"math"
	"time"
)

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

	if err = speakerEnable(true); err != nil {
		return err
	}
	if err = speakStart(); err != nil {
		return err
	}
	defer speakStop()

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
