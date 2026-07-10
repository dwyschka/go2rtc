package petkit

// AAC-LC encoder for the talkback backchannel: 16 kHz mono PCM -> ADTS-AAC-LC.
//
// A single_channel_element with a long (1024) window, per-band scalefactors,
// and spectral codebook 11 (HCB_ESC) for every band. No psychoacoustic model —
// uniform quantization, which is plenty for intelligible voice. The Huffman
// tables (aac_tables.go) and the cb11 pair/escape coding are the ISO/IEC
// 14496-3 tables/algorithm reproduced from the FAAC reference. The MDCT runs
// in O(N log N) via a 512-point complex FFT — the camera's ARM core cannot
// sustain the textbook O(N²) form in real time (see aacenc_mdct_test.go).
//
// Pure Go, no OS dependencies, so it is unit-testable on any platform.

import (
	"math"
	"math/bits"
	"os"

	"github.com/AlexxIT/go2rtc/pkg/aac"
)

// rawAAC strips the ADTS header so the ring carries raw AAC access units.
//
// Ground truth from the media daemon (media_arm, AXERA/ARM): its speaker
// decoder — AX_ADEC channel 0, created in FUN_00029380 — is opened with
// enTransType = 2 (TT_MP4_ADTS) and u32ConfLen = 0, i.e. it expects
// self-describing ADTS frames with no out-of-band ASC. The working
// notification-tone path (dispatch_handler_play_aac_file) streams full ADTS
// frames straight into the same channel, and the "auido-out" ring reader
// (FUN_0002914c) forwards each ring payload verbatim to AX_ADEC_SendStream. So
// ADTS is the correct default: raw AUs never sync in the ADTS decoder, which is
// why earlier raw frames were consumed but produced no PCM (silence).
// Set PETKIT_AAC_RAW=1 to strip the header for firmware opened in raw mode.
var rawAAC = os.Getenv("PETKIT_AAC_RAW") == "1"

// aacPayload returns the ring payload for one encoded frame: the full ADTS
// frame by default (what the media daemon's ADTS decoder needs), or the raw AAC
// AU (ADTS header stripped, honoring the CRC-present length) when PETKIT_AAC_RAW=1.
func aacPayload(adts []byte) []byte {
	if rawAAC && aac.IsADTS(adts) {
		if n := aac.ADTSHeaderLen(adts); len(adts) > n {
			return adts[n:]
		}
	}
	return adts
}

const (
	aacFrameSamples = 1024      // AAC-LC long-window frame length (N)
	mdctLen         = 2048      // MDCT input length (2N)
	targetBandIx    = 200       // per-band quantizer target: peak quantized value
	bandFloorRatio  = 0.02      // bands below this fraction of the global peak -> zeroed
	maxQuant        = 8191      // HCB_ESC escape range limit (MAX_HUFF_ESC_VAL)
	sfOffset        = 100       // AAC scalefactor gain offset (SF_OFFSET)
	escHCB          = 11        // spectral codebook we always use
	maxSfDelta      = 60        // scalefactor DPCM range (aacBook12 is centered at 60)
)

// 16 kHz long-window scalefactor-band widths (ISO/IEC 14496-3; from FAAC).
var swbWidth16 = []int{
	8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 12, 12, 12,
	12, 12, 12, 12, 12, 12, 16, 16, 16, 16, 20, 20, 20, 24,
	24, 28, 28, 32, 36, 40, 40, 44, 48, 52, 56, 60, 64, 64, 64,
}

// swbOffset16[i] = start spectral line of scalefactor band i (last entry = 1024).
var swbOffset16 []int

// The MDCT is evaluated as: fold the 2N-sample window into an N-point DCT-IV,
// then compute that DCT-IV with one N/2-point complex FFT plus pre/post
// rotations.
const (
	dct4Len = aacFrameSamples // N: DCT-IV length after folding the 2N window
	fftLen  = dct4Len / 2     // P: complex FFT size driving the DCT-IV
)

var (
	mdctWindow [mdctLen]float64        // sine analysis window
	fftTwid    [fftLen / 2]complex128  // e^(-j2πk/P) DFT twiddles
	preTwid    [fftLen]complex128      // e^(-jπi/N) DCT-IV pre-rotation
	postTwid   [fftLen]complex128      // e^(-jπ(4r+1)/(4N)) DCT-IV post-rotation
	bitrev     [fftLen]int             // FFT input permutation
)

func init() {
	swbOffset16 = make([]int, len(swbWidth16)+1)
	for i, w := range swbWidth16 {
		swbOffset16[i+1] = swbOffset16[i] + w
	}
	for n := 0; n < mdctLen; n++ {
		mdctWindow[n] = math.Sin(math.Pi / (2 * mdctLen) * (float64(n) + 0.5))
	}
	for k := range fftTwid {
		fftTwid[k] = expJ(-2 * math.Pi * float64(k) / fftLen)
	}
	for i := range preTwid {
		preTwid[i] = expJ(-math.Pi * float64(i) / dct4Len)
		postTwid[i] = expJ(-math.Pi * float64(4*i+1) / (4 * dct4Len))
	}
	log2n := bits.Len(uint(fftLen)) - 1
	for i := range bitrev {
		bitrev[i] = int(bits.Reverse16(uint16(i)) >> (16 - log2n))
	}
}

// expJ returns e^(jθ).
func expJ(theta float64) complex128 {
	return complex(math.Cos(theta), math.Sin(theta))
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

// mdct computes X[k] = Σ x[n]·cos[(π/2N)(2n+1+N/2)(2k+1)], the 2048-input /
// 1024-output forward MDCT. The direct form (kept as mdctDirect in the tests)
// needs ~2M float64 MACs per 64 ms frame — more than the camera's ARM core can
// sustain, which backs audio up in the sender queue as seconds of latency.
// This evaluates the same transform in O(N log N).
func mdct(x, out []float64) {
	const q = mdctLen / 4 // window quarter (a|b|c|d), 512 samples

	// Boundary folding: MDCT(a,b,c,d) = DCT-IV(-c_R - d, a - b_R).
	var u [dct4Len]float64
	for j := 0; j < q; j++ {
		u[j] = -x[3*q-1-j] - x[3*q+j]
		u[q+j] = x[j] - x[2*q-1-j]
	}

	// DCT-IV via one P-point FFT: pair samples from both ends into complex
	// values, pre-rotate, transform, post-rotate, deinterleave.
	var z [fftLen]complex128
	for i := 0; i < fftLen; i++ {
		z[bitrev[i]] = complex(u[2*i], u[dct4Len-1-2*i]) * preTwid[i]
	}
	fftInPlace(z[:])
	for r := 0; r < fftLen; r++ {
		w := z[r] * postTwid[r]
		out[2*r] = real(w)
		out[dct4Len-1-2*r] = -imag(w)
	}
}

// fftInPlace is an iterative radix-2 DIT FFT; input must already be in
// bit-reversed order (callers index through bitrev).
func fftInPlace(z []complex128) {
	n := len(z)
	for size := 2; size <= n; size <<= 1 {
		half, step := size>>1, n/size
		for base := 0; base < n; base += size {
			for i := 0; i < half; i++ {
				t := z[base+half+i] * fftTwid[i*step]
				z[base+half+i] = z[base+i] - t
				z[base+i] += t
			}
		}
	}
}

// quantize chooses a scalefactor per band so each audible band keeps good
// precision. A single global scalefactor (the old approach) is set by the
// loudest bin, so quiet bands — the formant detail and high-frequency energy
// that make speech intelligible — collapse into quantization noise. It sounds
// fine for a pure tone (one band) but buzzy for voice. Per-band scalefactors,
// DPCM-coded, are the standard AAC-LC fix. Returns the transmitted scalefactor
// per band, the quantized spectrum, and the highest occupied band.
func quantize(spec []float64) (sf []int, ix [aacFrameSamples]int, maxSfb int) {
	var globalMax float64
	for _, v := range spec {
		if a := math.Abs(v); a > globalMax {
			globalMax = a
		}
	}
	if globalMax < 1e-6 {
		return nil, ix, 0 // silence
	}
	floor := globalMax * bandFloorRatio

	// Desired scalefactor per band (only for bands above the floor).
	nb := len(swbWidth16)
	desired := make([]int, nb)
	active := make([]bool, nb)
	firstActive := -1
	for sfb := 0; sfb < nb; sfb++ {
		var m float64
		for k := swbOffset16[sfb]; k < swbOffset16[sfb+1]; k++ {
			if a := math.Abs(spec[k]); a > m {
				m = a
			}
		}
		if m < floor {
			continue
		}
		active[sfb] = true
		if firstActive < 0 {
			firstActive = sfb
		}
		// ix = (|xr|·2^(-0.25(sf-100)))^0.75 ; pick sf so band peak ≈ targetBandIx.
		// => sf = 100 - log2(target / m^0.75) / 0.1875
		desired[sfb] = clampSf(int(math.Round(
			float64(sfOffset) - math.Log2(float64(targetBandIx)/math.Pow(m, 0.75))/0.1875)))
	}
	if firstActive < 0 {
		return nil, ix, 0
	}
	for sfb := nb - 1; sfb >= 0; sfb-- {
		if active[sfb] {
			maxSfb = sfb + 1
			break
		}
	}

	// Assign transmitted scalefactors (DPCM deltas clamped to ±maxSfDelta so the
	// scalefactor Huffman book can code them) and quantize each active band.
	// Inactive bands carry the previous scalefactor (delta 0) and stay zero.
	sf = make([]int, maxSfb)
	prev := desired[firstActive]
	for sfb := 0; sfb < maxSfb; sfb++ {
		if !active[sfb] {
			sf[sfb] = prev
			continue
		}
		d := desired[sfb] - prev
		if d < -maxSfDelta {
			d = -maxSfDelta
		} else if d > maxSfDelta {
			d = maxSfDelta
		}
		s := prev + d
		sf[sfb] = s

		gain := math.Pow(2, -0.25*float64(s-sfOffset))
		for k := swbOffset16[sfb]; k < swbOffset16[sfb+1]; k++ {
			q := int(math.Pow(math.Abs(spec[k])*gain, 0.75) + 0.4054)
			if q > maxQuant {
				q = maxQuant
			}
			if spec[k] < 0 {
				q = -q
			}
			ix[k] = q
		}
		prev = s
	}
	return sf, ix, maxSfb
}

func clampSf(s int) int {
	if s < 0 {
		return 0
	}
	if s > 255 {
		return 255
	}
	return s
}

// encodeSCE builds a raw_data_block: one single_channel_element + END.
func encodeSCE(sf []int, ix []int, maxSfb int) []byte {
	bw := &bitWriter{}

	bw.write(0, 3) // id_syn_ele = ID_SCE
	bw.write(0, 4) // element_instance_tag

	globalGain := 0
	if maxSfb > 0 {
		globalGain = sf[0]
	}
	bw.write(uint32(globalGain), 8) // global_gain (the first scalefactor)

	// ics_info (long window)
	bw.write(0, 1)              // ics_reserved_bit
	bw.write(0, 2)              // window_sequence = ONLY_LONG_SEQUENCE
	bw.write(0, 1)              // window_shape = sine
	bw.write(uint32(maxSfb), 6) // max_sfb
	bw.write(0, 1)              // predictor_data_present

	if maxSfb > 0 {
		// section_data: one section, codebook 11, covering all maxSfb bands.
		bw.write(escHCB, 4) // sect_cb
		run := maxSfb
		for run >= 31 {
			bw.write(31, 5) // sect_len_incr escape
			run -= 31
		}
		bw.write(uint32(run), 5)

		// scale_factor_data: DPCM against the previous scalefactor (the first band
		// is coded relative to global_gain, i.e. delta 0).
		prev := globalGain
		for sfb := 0; sfb < maxSfb; sfb++ {
			delta := sf[sfb] - prev // guaranteed within ±maxSfDelta by quantize
			bw.write(aacBook12[delta+maxSfDelta][1], uint8(aacBook12[delta+maxSfDelta][0]))
			prev = sf[sfb]
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
