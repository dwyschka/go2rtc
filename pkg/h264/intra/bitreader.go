// Package intra is a minimal, pure-Go H.264 decoder for a single intra (IDR)
// keyframe. It exists to turn the H.264 keyframes the petkit driver already
// reads out of the camera's shared-memory ring into a still image (JPEG) without
// ffmpeg and without touching the camera's live encode pipeline.
//
// Scope is deliberately narrow: one IDR picture, Main profile, 4:2:0, 4x4
// transform only, CABAC or CAVLC entropy, progressive frames, no FMO/ASO. That
// is exactly what the Petkit AXERA cameras emit. Inter prediction, reference
// frames, 8x8 transform, interlacing and scaling lists are out of scope.
package intra

// bitReader reads bits MSB-first from an H.264 RBSP (emulation-prevention bytes
// already removed). Reads past the end return zero bits rather than panicking so
// malformed device data can never crash the process.
type bitReader struct {
	data []byte
	pos  int // absolute bit position
}

func newBitReader(b []byte) *bitReader { return &bitReader{data: b} }

func (r *bitReader) readBit() uint32 {
	i := r.pos >> 3
	if i >= len(r.data) {
		r.pos++
		return 0
	}
	bit := uint32(r.data[i]>>(7-uint(r.pos&7))) & 1
	r.pos++
	return bit
}

func (r *bitReader) readBits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v = (v << 1) | r.readBit()
	}
	return v
}

func (r *bitReader) readFlag() bool { return r.readBit() == 1 }

// readUE decodes an unsigned Exp-Golomb code (ue(v)).
func (r *bitReader) readUE() uint32 {
	zeros := 0
	for r.readBit() == 0 {
		zeros++
		if zeros > 31 { // malformed / past end: stop runaway
			return 0
		}
	}
	if zeros == 0 {
		return 0
	}
	return (1 << uint(zeros)) - 1 + r.readBits(zeros)
}

// readSE decodes a signed Exp-Golomb code (se(v)).
func (r *bitReader) readSE() int {
	k := r.readUE()
	if k&1 == 1 {
		return int((k + 1) / 2)
	}
	return -int(k / 2)
}

// byteAligned reports whether the reader is on a byte boundary.
func (r *bitReader) byteAligned() bool { return r.pos&7 == 0 }

// bitsLeft returns how many bits remain in the buffer.
func (r *bitReader) bitsLeft() int { return len(r.data)*8 - r.pos }

// moreRBSPData reports whether there is more RBSP data before the trailing
// rbsp_stop_one_bit (7.2). Used to detect the optional PPS extension.
func (r *bitReader) moreRBSPData() bool {
	if r.bitsLeft() <= 0 {
		return false
	}
	// Find the last set bit in the buffer; RBSP ends with a 1 stop bit followed
	// by zero padding. If the current position is before that stop bit, there is
	// more data.
	last := -1
	for i := len(r.data)*8 - 1; i >= r.pos; i-- {
		if r.data[i>>3]>>(7-uint(i&7))&1 == 1 {
			last = i
			break
		}
	}
	return last > r.pos
}
