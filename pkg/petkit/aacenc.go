package petkit

// AAC-LC encoder for the talkback backchannel: 16 kHz mono PCM -> ADTS-AAC-LC.
//
// A single_channel_element with a long (1024) window, one global scalefactor,
// and spectral codebook 11 (HCB_ESC) for every band. No psychoacoustic model —
// uniform quantization, which is plenty for intelligible voice. The Huffman
// tables (aac_tables.go) and the cb11 pair/escape coding are the ISO/IEC
// 14496-3 tables/algorithm reproduced from the FAAC reference.
//
// Pure Go, no OS dependencies, so it is unit-testable on any platform.

import (
	"math"
	"math/bits"
	"os"
)

// rawAAC strips the ADTS header so the ring carries raw AAC access units. The
// media daemon's decoder is TT_MP4_ADTS (so ADTS is the default), but set
// PETKIT_AAC_RAW=1 to try raw AU without a rebuild if that turns out wrong.
var rawAAC = os.Getenv("PETKIT_AAC_RAW") == "1"

// aacPayload returns the ring payload for one encoded frame: the ADTS frame as
// produced, or the raw AAC AU (7-byte header stripped) when PETKIT_AAC_RAW=1.
func aacPayload(adts []byte) []byte {
	if rawAAC && len(adts) > 7 {
		return adts[7:]
	}
	return adts
}

const (
	aacFrameSamples = 1024      // AAC-LC long-window frame length (N)
	mdctLen         = 2048      // MDCT input length (2N)
	targetMaxIx     = 500       // quantizer target: peak quantized value
	maxQuant        = 8191      // HCB_ESC escape range limit (MAX_HUFF_ESC_VAL)
	sfOffset        = 100       // AAC scalefactor gain offset (SF_OFFSET)
	escHCB          = 11        // spectral codebook we always use
)

// 16 kHz long-window scalefactor-band widths (ISO/IEC 14496-3; from FAAC).
var swbWidth16 = []int{
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 12, 12, 12,
	12, 12, 12, 12, 12, 12, 16, 16, 16, 16, 20, 20, 20, 24,
	24, 28, 28, 32, 36, 40, 40, 44, 48, 52, 56, 60, 64, 64, 64,
}

// swbOffset16[i] = start spectral line of scalefactor band i (last entry = 1024).
var swbOffset16 []int

var (
	mdctWindow [mdctLen]float64 // sine analysis window
	mdctCos    [8192]float64    // cos(pi*i/4096) lookup for the direct MDCT
)

func init() {
	swbOffset16 = make([]int, len(swbWidth16)+1)
	for i, w := range swbWidth16 {
		swbOffset16[i+1] = swbOffset16[i] + w
	}
	for n := 0; n < mdctLen; n++ {
		mdctWindow[n] = math.Sin(math.Pi / (2 * mdctLen) * (float64(n) + 0.5))
	}
	for i := range mdctCos {
		mdctCos[i] = math.Cos(math.Pi * float64(i) / 4096)
	}
}

func sampleRateIndex(rate int) byte {
	switch rate {
	case 96000:
		return 0
	case 88200:
		return 1
	case 64000:
		return 2
	case 48000:
		return 3
	case 44100:
		return 4
	case 32000:
		return 5
	case 24000:
		return 6
	case 22050:
		return 7
	case 16000:
		return 8
	case 12000:
		return 9
	case 11025:
		return 10
	case 8000:
		return 11
	default:
		return 8
	}
}

type aacEncoder struct {
	rateIdx  byte
	channels byte
	prev     [aacFrameSamples]float64 // previous frame, for 50% MDCT overlap
}

func newAACEncoder(sampleRate, channels int) *aacEncoder {
	return &aacEncoder{rateIdx: sampleRateIndex(sampleRate), channels: byte(channels)}
}

// EncodeFrame encodes one 1024-sample mono frame to a single ADTS-AAC-LC frame.
func (e *aacEncoder) EncodeFrame(pcm []int16) []byte {
	if len(pcm) < aacFrameSamples {
		return nil
	}

	// Windowed MDCT over [prev block | current block].
	var win [mdctLen]float64
	for n := 0; n < aacFrameSamples; n++ {
		win[n] = e.prev[n] * mdctWindow[n]
		win[n+aacFrameSamples] = float64(pcm[n]) * mdctWindow[n+aacFrameSamples]
	}
	for n := 0; n < aacFrameSamples; n++ {
		e.prev[n] = float64(pcm[n])
	}

	var spec [aacFrameSamples]float64
	mdct(win[:], spec[:])

	sf, ix, maxSfb := quantize(spec[:])

	raw := encodeSCE(sf, ix[:], maxSfb)
	return e.addADTS(raw)
}

// mdct computes X[k] = Σ x[n]·cos[(π/N)(n+½+N/2)(k+½)], N=1024, via a lookup
// table so the hot loop has no transcendental calls.
func mdct(x, out []float64) {
	for k := 0; k < aacFrameSamples; k++ {
		k2 := 2*k + 1
		var sum float64
		for n := 0; n < mdctLen; n++ {
			idx := ((2*n + 1 + aacFrameSamples) * k2) & 8191
			sum += x[n] * mdctCos[idx]
		}
		out[k] = sum
	}
}

// quantize applies a single global scalefactor chosen so the peak quantized
// value is near targetMaxIx, then returns the scalefactor, the quantized
// spectrum, and the highest occupied scalefactor band.
func quantize(spec []float64) (sf int, ix [aacFrameSamples]int, maxSfb int) {
	var maxAbs float64
	for _, v := range spec {
		if a := math.Abs(v); a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs < 1e-6 {
		return sfOffset, ix, 0 // silence
	}

	// ix = (|xr|·2^(-0.25(sf-100)))^0.75 ; pick sf so peak ix ≈ targetMaxIx.
	// => sf = 100 - log2(target / maxAbs^0.75) / 0.1875
	sfF := float64(sfOffset) - math.Log2(float64(targetMaxIx)/math.Pow(maxAbs, 0.75))/0.1875
	sf = int(math.Round(sfF))
	if sf < 0 {
		sf = 0
	}
	if sf > 255 {
		sf = 255
	}

	gain := math.Pow(2, -0.1875*float64(sf-sfOffset))
	for k, v := range spec {
		q := int(math.Pow(math.Abs(v)*gain, 0.75) + 0.4054)
		if q > maxQuant {
			q = maxQuant
		}
		if v < 0 {
			q = -q
		}
		ix[k] = q
	}

	// Highest scalefactor band containing a non-zero coefficient.
	for sfb := len(swbWidth16) - 1; sfb >= 0; sfb-- {
		nonzero := false
		for k := swbOffset16[sfb]; k < swbOffset16[sfb+1]; k++ {
			if ix[k] != 0 {
				nonzero = true
				break
			}
		}
		if nonzero {
			maxSfb = sfb + 1
			break
		}
	}
	return sf, ix, maxSfb
}

// encodeSCE builds a raw_data_block: one single_channel_element + END.
func encodeSCE(sf int, ix []int, maxSfb int) []byte {
	bw := &bitWriter{}

	bw.write(0, 3) // id_syn_ele = ID_SCE
	bw.write(0, 4) // element_instance_tag

	bw.write(uint32(sf), 8) // global_gain (the first scalefactor)

	// ics_info (long window)
	bw.write(0, 1)                // ics_reserved_bit
	bw.write(0, 2)                // window_sequence = ONLY_LONG_SEQUENCE
	bw.write(0, 1)                // window_shape = sine
	bw.write(uint32(maxSfb), 6)   // max_sfb
	bw.write(0, 1)                // predictor_data_present

	if maxSfb > 0 {
		// section_data: one section, codebook 11, covering all maxSfb bands.
		bw.write(escHCB, 4) // sect_cb
		run := maxSfb
		for run >= 31 {
			bw.write(31, 5) // sect_len_incr escape
			run -= 31
		}
		bw.write(uint32(run), 5)

		// scale_factor_data: every band uses the same scalefactor (= global_gain),
		// so each DPCM delta is 0 -> book12[60].
		for sfb := 0; sfb < maxSfb; sfb++ {
			bw.write(aacBook12[60][1], uint8(aacBook12[60][0]))
		}
	}

	bw.write(0, 1) // pulse_data_present
	bw.write(0, 1) // tns_data_present
	bw.write(0, 1) // gain_control_data_present

	// spectral_data: cb11 pairs for every band.
	for sfb := 0; sfb < maxSfb; sfb++ {
		for k := swbOffset16[sfb]; k < swbOffset16[sfb+1]; k += 2 {
			writeCB11Pair(bw, ix[k], ix[k+1])
		}
	}

	bw.write(7, 3) // id_syn_ele = ID_END
	bw.align()
	return bw.bytes()
}

// writeCB11Pair encodes one (x0,x1) spectral pair with codebook 11: the Huffman
// codeword for the clipped magnitudes, sign bits, then escape suffixes for any
// magnitude >= 16. Ported verbatim from the FAAC HCB_ESC path.
func writeCB11Pair(bw *bitWriter, x0, x1 int) {
	a0, a1 := abs(x0), abs(x1)
	v0, v1 := a0, a1
	if v0 > 16 {
		v0 = 16
	}
	if v1 > 16 {
		v1 = 16
	}
	entry := aacBook11[17*v0+v1]
	blen := uint8(entry[0])
	data := entry[1]
	if x0 != 0 {
		blen++
		data = data<<1 | b2u(x0 < 0)
	}
	if x1 != 0 {
		blen++
		data = data<<1 | b2u(x1 < 0)
	}
	bw.write(data, blen)

	if a0 >= 16 {
		code, n := escape(a0)
		bw.write(code, n)
	}
	if a1 >= 16 {
		code, n := escape(a1)
		bw.write(code, n)
	}
}

// escape returns the HCB_ESC escape suffix (code, bit length) for magnitude x>=16.
func escape(x int) (uint32, uint8) {
	preflen := 31 - bits.LeadingZeros32(uint32(x)) - 4
	base := 1 << (preflen + 4)
	code := uint32((1<<(preflen+1))-2)<<(preflen+4) | uint32(x-base)
	return code, uint8((preflen + 1) + (preflen + 4))
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// addADTS prepends a 7-byte ADTS header (no CRC) for AAC-LC.
func (e *aacEncoder) addADTS(raw []byte) []byte {
	const adtsLen = 7
	frameLen := adtsLen + len(raw)

	out := make([]byte, frameLen)
	out[0] = 0xFF
	out[1] = 0xF1 // MPEG-4, layer 0, no CRC
	out[2] = 1<<6 | (e.rateIdx&0x0F)<<2 | (e.channels>>2)&0x1
	out[3] = (e.channels&0x3)<<6 | byte(frameLen>>11)&0x3
	out[4] = byte(frameLen >> 3)
	out[5] = byte(frameLen<<5)&0xE0 | 0x1F
	out[6] = 0xFC
	copy(out[adtsLen:], raw)
	return out
}

// bitWriter is an MSB-first bit accumulator.
type bitWriter struct {
	buf  []byte
	cur  byte
	nbit uint8
}

func (w *bitWriter) write(value uint32, nbits uint8) {
	for nbits > 0 {
		nbits--
		w.cur = w.cur<<1 | byte((value>>nbits)&1)
		w.nbit++
		if w.nbit == 8 {
			w.buf = append(w.buf, w.cur)
			w.cur = 0
			w.nbit = 0
		}
	}
}

func (w *bitWriter) align() {
	if w.nbit > 0 {
		w.cur <<= 8 - w.nbit
		w.buf = append(w.buf, w.cur)
		w.cur = 0
		w.nbit = 0
	}
}

func (w *bitWriter) bytes() []byte { return w.buf }
