package intra

// CABAC arithmetic decoding engine (H.264 clause 9.3.3.2). Decodes bins from the
// slice data; the syntax layer (mb.go) maps context indices to these calls.

// cabac holds the arithmetic decoder state plus the per-context probability
// models (pStateIdx 0..63 and valMPS) for the whole slice.
type cabac struct {
	r *bitReader

	codIRange  uint32
	codIOffset uint32

	// context models, indexed by ctxIdx (see ctx.go for the layout).
	state [numCtx]uint8 // pStateIdx
	mps   [numCtx]uint8 // valMPS
}

// initEngine performs 9.3.1.2 arithmetic decoding engine initialization. The
// reader must already be byte-aligned at the start of the slice data.
func (c *cabac) initEngine(r *bitReader) {
	c.r = r
	c.codIRange = 510
	c.codIOffset = r.readBits(9)
}

// decodeDecision decodes one regular bin using context ctxIdx (9.3.3.2.1).
func (c *cabac) decodeDecision(ctxIdx int) uint32 {
	pState := c.state[ctxIdx]
	q := (c.codIRange >> 6) & 3
	rLPS := uint32(rangeTabLPS[pState][q])
	c.codIRange -= rLPS

	var binVal uint32
	if c.codIOffset >= c.codIRange {
		binVal = uint32(1 - c.mps[ctxIdx])
		c.codIOffset -= c.codIRange
		c.codIRange = rLPS
		if pState == 0 {
			c.mps[ctxIdx] = 1 - c.mps[ctxIdx]
		}
		c.state[ctxIdx] = transIdxLPS[pState]
	} else {
		binVal = uint32(c.mps[ctxIdx])
		c.state[ctxIdx] = transIdxMPS[pState]
	}

	c.renorm()
	return binVal
}

// decodeBypass decodes one bypass bin (9.3.3.2.3).
func (c *cabac) decodeBypass() uint32 {
	c.codIOffset = (c.codIOffset << 1) | c.r.readBit()
	if c.codIOffset >= c.codIRange {
		c.codIOffset -= c.codIRange
		return 1
	}
	return 0
}

// decodeTerminate decodes the end_of_slice / terminate bin (9.3.3.2.4). Returns
// 1 when the slice terminates.
func (c *cabac) decodeTerminate() uint32 {
	c.codIRange -= 2
	if c.codIOffset >= c.codIRange {
		return 1
	}
	c.renorm()
	return 0
}

// renorm is the renormalization process (9.3.3.2.2).
func (c *cabac) renorm() {
	for c.codIRange < 256 {
		c.codIRange <<= 1
		c.codIOffset = (c.codIOffset << 1) | c.r.readBit()
	}
}

// decodeBypassBits decodes n bypass bins as an unsigned value (MSB first).
func (c *cabac) decodeBypassBits(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v = (v << 1) | c.decodeBypass()
	}
	return v
}

// rangeTabLPS is Table 9-44: codIRangeLPS given pStateIdx and (codIRange>>6)&3.
var rangeTabLPS = [64][4]uint8{
	{128, 176, 208, 240}, {128, 167, 197, 227}, {128, 158, 187, 216}, {123, 150, 178, 205},
	{116, 142, 169, 195}, {111, 135, 160, 185}, {105, 128, 152, 175}, {100, 122, 144, 166},
	{95, 116, 137, 158}, {90, 110, 130, 150}, {85, 104, 123, 142}, {81, 99, 117, 135},
	{77, 94, 111, 128}, {73, 89, 105, 122}, {69, 85, 100, 116}, {66, 80, 95, 110},
	{62, 76, 90, 104}, {59, 72, 86, 99}, {56, 69, 81, 94}, {53, 65, 77, 89},
	{51, 62, 73, 85}, {48, 59, 69, 80}, {46, 56, 66, 76}, {43, 53, 63, 72},
	{41, 50, 59, 69}, {39, 48, 56, 65}, {37, 45, 54, 62}, {35, 43, 51, 59},
	{33, 41, 48, 56}, {32, 39, 46, 53}, {30, 37, 43, 50}, {28, 35, 41, 48},
	{27, 33, 39, 45}, {26, 31, 37, 43}, {24, 30, 35, 41}, {23, 28, 33, 39},
	{22, 27, 32, 37}, {21, 26, 30, 35}, {20, 24, 29, 33}, {19, 23, 27, 31},
	{18, 22, 26, 30}, {17, 21, 25, 28}, {16, 20, 23, 27}, {15, 19, 22, 25},
	{14, 18, 21, 24}, {14, 17, 20, 23}, {13, 16, 19, 22}, {12, 15, 18, 21},
	{12, 14, 17, 20}, {11, 14, 16, 19}, {11, 13, 15, 18}, {10, 12, 15, 17},
	{10, 12, 14, 16}, {9, 11, 13, 15}, {9, 11, 12, 14}, {8, 10, 12, 14},
	{8, 9, 11, 13}, {7, 9, 11, 12}, {7, 9, 10, 12}, {7, 8, 10, 11},
	{6, 8, 9, 11}, {6, 7, 9, 10}, {6, 7, 8, 9}, {2, 2, 2, 2},
}

// transIdxLPS / transIdxMPS are Table 9-45 state transitions.
var transIdxLPS = [64]uint8{
	0, 0, 1, 2, 2, 4, 4, 5, 6, 7, 8, 9, 9, 11, 11, 12,
	13, 13, 15, 15, 16, 16, 18, 18, 19, 19, 21, 21, 23, 22, 23, 24,
	24, 25, 26, 26, 27, 27, 28, 29, 29, 30, 30, 30, 31, 32, 32, 33,
	33, 33, 34, 34, 35, 35, 35, 36, 36, 36, 37, 37, 37, 38, 38, 63,
}

var transIdxMPS = [64]uint8{
	1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
	33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48,
	49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, 62, 63,
}
