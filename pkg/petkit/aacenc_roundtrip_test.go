package petkit

import (
	"bytes"
	"encoding/binary"
	"math"
	"os/exec"
	"testing"
)

// TestAACRoundtrip encodes a 1 kHz sine to AAC-LC, decodes it with ffmpeg, and
// checks the recovered signal is still a 1 kHz tone — an end-to-end check that
// our bitstream is valid AAC and reproduces the input. Skips without ffmpeg.
func TestAACRoundtrip(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}

	enc := newAACEncoder(16000, 1)
	var adts []byte
	phase := 0.0
	for f := 0; f < 8; f++ {
		pcm := make([]int16, aacFrameSamples)
		for i := range pcm {
			pcm[i] = int16(12000 * math.Sin(phase))
			phase += 2 * math.Pi * 1000 / 16000
		}
		adts = append(adts, enc.EncodeFrame(pcm)...)
	}

	cmd := exec.Command(ff, "-loglevel", "error", "-f", "aac", "-i", "pipe:0", "-f", "s16le", "pipe:1")
	cmd.Stdin = bytes.NewReader(adts)
	pcmOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffmpeg decode failed: %v", err)
	}

	n := len(pcmOut) / 2
	if n < 2*aacFrameSamples {
		t.Fatalf("decoded only %d samples", n)
	}
	samples := make([]float64, n)
	for i := 0; i < n; i++ {
		samples[i] = float64(int16(binary.LittleEndian.Uint16(pcmOut[2*i:])))
	}
	samples = samples[aacFrameSamples:] // skip MDCT warm-up frame

	goertzel := func(f float64) float64 {
		w := 2 * math.Pi * f / 16000
		c := 2 * math.Cos(w)
		var s1, s2 float64
		for _, x := range samples {
			s0 := x + c*s1 - s2
			s2, s1 = s1, s0
		}
		return s1*s1 + s2*s2 - c*s1*s2
	}
	e1k := goertzel(1000)
	eOther := goertzel(500) + goertzel(2000) + goertzel(3000)
	if e1k < 50*eOther {
		t.Fatalf("1 kHz not dominant: e1k=%.0f eOther=%.0f", e1k, eOther)
	}
}
