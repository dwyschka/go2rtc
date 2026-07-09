package petkit

// The FFT-based mdct must produce the same spectrum as the textbook direct
// form it replaced. The direct form lives here as the reference oracle: it is
// trivially correct (it is the MDCT definition), but at ~2M float64 MACs per
// 64 ms frame it was too slow for real time on the camera's ARM core.

import (
	"math"
	"math/rand"
	"testing"
)

// mdctCosLUT is the former production cos(π·i/4096) lookup table.
var mdctCosLUT = func() []float64 {
	lut := make([]float64, 8192)
	for i := range lut {
		lut[i] = math.Cos(math.Pi * float64(i) / 4096)
	}
	return lut
}()

// mdctDirect is the former production implementation, verbatim: the MDCT
// definition X[k] = Σ x[n]·cos[(π/2N)(2n+1+N/2)(2k+1)] with a cosine lookup.
func mdctDirect(x, out []float64) {
	for k := 0; k < aacFrameSamples; k++ {
		k2 := 2*k + 1
		var sum float64
		for n := 0; n < mdctLen; n++ {
			idx := ((2*n + 1 + aacFrameSamples) * k2) & 8191
			sum += x[n] * mdctCosLUT[idx]
		}
		out[k] = sum
	}
}

func TestMDCTMatchesDirectForm(t *testing.T) {
	cases := map[string]func(i int) float64{
		"impulse": func(i int) float64 {
			if i == 137 {
				return 30000
			}
			return 0
		},
		"dc":   func(int) float64 { return 12345 },
		"sine": func(i int) float64 { return 20000 * math.Sin(2*math.Pi*440*float64(i)/16000) },
		"random": func() func(int) float64 {
			rng := rand.New(rand.NewSource(1))
			return func(int) float64 { return float64(rng.Intn(65536) - 32768) }
		}(),
	}

	for name, gen := range cases {
		var x [mdctLen]float64
		for i := range x {
			x[i] = gen(i)
		}

		var want, got [aacFrameSamples]float64
		mdctDirect(x[:], want[:])
		mdct(x[:], got[:])

		// Scale the tolerance to the spectrum's magnitude: both forms carry
		// float64 rounding, the direct form via its 2048-term sums.
		var peak float64
		for _, v := range want {
			if a := math.Abs(v); a > peak {
				peak = a
			}
		}
		tol := math.Max(peak*1e-9, 1e-6)

		for k := range want {
			if d := math.Abs(got[k] - want[k]); d > tol {
				t.Fatalf("%s: bin %d: fft=%g direct=%g diff=%g (tol %g)",
					name, k, got[k], want[k], d, tol)
			}
		}
	}
}

func BenchmarkMDCT(b *testing.B) {
	var x [mdctLen]float64
	rng := rand.New(rand.NewSource(1))
	for i := range x {
		x[i] = float64(rng.Intn(65536) - 32768)
	}
	var out [aacFrameSamples]float64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mdct(x[:], out[:])
	}
}

func BenchmarkMDCTDirect(b *testing.B) {
	var x [mdctLen]float64
	rng := rand.New(rand.NewSource(1))
	for i := range x {
		x[i] = float64(rng.Intn(65536) - 32768)
	}
	var out [aacFrameSamples]float64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mdctDirect(x[:], out[:])
	}
}

// BenchmarkEncodeFrame measures the full per-frame encode cost — the number
// that must stay well under the 64 ms real-time budget on the device.
func BenchmarkEncodeFrame(b *testing.B) {
	enc := newAACEncoder(16000, 1)
	pcm := make([]int16, aacFrameSamples)
	for i := range pcm {
		pcm[i] = int16(10000 * math.Sin(2*math.Pi*300*float64(i)/16000))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		enc.EncodeFrame(pcm)
	}
}
