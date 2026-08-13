package intra

// decodeSliceIDR runs the CABAC slice-data loop over every macroblock of the
// IDR picture. Only the syntax parse + coefficient storage happens here;
// reconstruction (intra prediction + transform) is applied per MB as it goes.
func (d *decoder) decodeSliceIDR(rbsp []byte) error {
	h, r, err := parseSliceHeaderI(rbsp, d.sps, d.pps, 3)
	if err != nil {
		return err
	}
	d.qp = h.sliceQP
	d.c.initContexts(h.sliceQP)
	d.c.initEngine(r)

	total := d.mbW * d.mbH
	for addr := int(h.firstMB); addr < total; addr++ {
		if err := d.decodeMB(addr); err != nil {
			return err
		}
		if d.c.decodeTerminate() == 1 {
			break
		}
	}
	return nil
}

// neighbour macroblock lookup (raster). Returns index or -1 if unavailable.
func (d *decoder) mbLeft(addr int) int {
	if addr%d.mbW == 0 {
		return -1
	}
	return addr - 1
}
func (d *decoder) mbTop(addr int) int {
	if addr < d.mbW {
		return -1
	}
	return addr - d.mbW
}

// decodeMB decodes one macroblock (Intra only).
func (d *decoder) decodeMB(addr int) error {
	mb := &d.mbs[addr]
	mb.available = true
	mbx, mby := addr%d.mbW, addr/d.mbW

	mbType, err := d.decodeMBTypeI(addr)
	if err != nil {
		return err
	}
	if mbType == 25 {
		return errUnsupported // I_PCM not supported
	}

	var cbpLuma, cbpChroma int
	if mbType == 0 {
		// I_NxN (Intra_4x4): per-block prediction modes.
		mb.i16x16 = false
		d.decodeIntra4x4Modes(addr)
		mb.chromaMode = d.decodeChromaPredMode(addr)
		cbp := d.decodeCBP(addr)
		mb.cbp = cbp
		cbpLuma = cbp & 15
		cbpChroma = cbp >> 4
	} else {
		// I_16x16: prediction mode + CBP are folded into mb_type (1..24).
		mb.i16x16 = true
		t := mbType - 1
		mb.i16Mode = t % 4
		cbpLuma = 0
		if (t/4)%2 == 1 {
			cbpLuma = 15
		}
		cbpChroma = t / 12 // 0,1,2
		mb.chromaMode = d.decodeChromaPredMode(addr)
		mb.cbp = cbpLuma | (cbpChroma << 4)
	}

	// mb_qp_delta is present when the MB codes any residual (cbp != 0) or is
	// Intra_16x16.
	if cbpLuma != 0 || cbpChroma != 0 || mb.i16x16 {
		dqp := d.decodeMBQPDelta(addr)
		d.qp = ((d.qp + dqp + 52 + 2*0) % 52) // QpBdOffsetY=0
		if d.qp < 0 {
			d.qp += 52
		}
	}
	mb.qp = d.qp

	// residuals
	if err := d.decodeResiduals(addr, cbpLuma, cbpChroma); err != nil {
		return err
	}

	// reconstruction happens in recon.go (added next stage); for now the parse
	// walks the whole slice so the structure can be validated.
	_ = mbx
	_ = mby
	return nil
}

// decodeMBTypeI decodes mb_type for an I-slice (Table 9-36 binarization,
// ctxIdxOffset 3). Returns 0 (I_NxN), 1..24 (I_16x16) or 25 (I_PCM).
func (d *decoder) decodeMBTypeI(addr int) (int, error) {
	// bin0 ctxIdxInc = condTermFlagA + condTermFlagB, condTermFlagN = 1 if
	// neighbour available and NOT I_NxN (9.3.3.1.1.3).
	inc := 0
	if l := d.mbLeft(addr); l >= 0 && d.mbs[l].available && (d.mbs[l].i16x16 || d.mbs[l].iPCM) {
		inc++
	}
	if t := d.mbTop(addr); t >= 0 && d.mbs[t].available && (d.mbs[t].i16x16 || d.mbs[t].iPCM) {
		inc++
	}
	if d.c.decodeDecision(ctxMBTypeI+inc) == 0 {
		return 0, nil // I_NxN
	}
	if d.c.decodeTerminate() == 1 {
		return 25, nil // I_PCM
	}
	// I_16x16 suffix bins. Their ctxIdxInc runs sequentially 3,4,5,6,7 by bin
	// POSITION (Table 9-39), and the chroma-cbp part is 1 or 2 bins, so the
	// prediction-mode bins shift context when chroma cbp is 0. Use a running inc.
	mbType := 1
	inc3 := 3
	next := func() uint32 {
		v := d.c.decodeDecision(ctxMBTypeI + inc3)
		inc3++
		return v
	}
	// CodedBlockPatternLuma (0 or 15) -> +12
	if next() == 1 {
		mbType += 12
	}
	// CodedBlockPatternChroma (0,1,2) -> +4 per unit (1 or 2 bins)
	if next() == 1 {
		if next() == 1 {
			mbType += 8
		} else {
			mbType += 4
		}
	}
	// Intra16x16PredMode (0..3) -> 2 bins
	if next() == 1 {
		mbType += 2
	}
	if next() == 1 {
		mbType++
	}
	return mbType, nil
}

// decodeIntra4x4Modes decodes the 16 per-4x4-block prediction modes.
func (d *decoder) decodeIntra4x4Modes(addr int) {
	mb := &d.mbs[addr]
	for i := 0; i < 16; i++ {
		predMode := d.predIntra4x4Mode(addr, i)
		if d.c.decodeDecision(ctxPrevIntra4x4Flag) == 1 {
			mb.i4Modes[i] = int8(predMode)
			continue
		}
		rem := d.c.decodeDecision(ctxRemIntra4x4Mode)
		rem |= d.c.decodeDecision(ctxRemIntra4x4Mode) << 1
		rem |= d.c.decodeDecision(ctxRemIntra4x4Mode) << 2
		m := int(rem)
		if m < predMode {
			mb.i4Modes[i] = int8(m)
		} else {
			mb.i4Modes[i] = int8(m + 1)
		}
	}
}

// predIntra4x4Mode returns the predicted Intra4x4PredMode = min of the left and
// top block modes (8.3.1.1); unavailable neighbours or non-Intra4x4 give DC(2).
func (d *decoder) predIntra4x4Mode(addr, blk int) int {
	left := d.neighbour4x4Mode(addr, blk, true)
	top := d.neighbour4x4Mode(addr, blk, false)
	if left < top {
		return left
	}
	return top
}

// neighbour4x4Mode returns the Intra4x4PredMode of the block to the left (or
// top) of block blk, or 2 (DC) if unavailable.
func (d *decoder) neighbour4x4Mode(addr, blk int, leftDir bool) int {
	// 4x4 block raster position within the MB (0..3, 0..3).
	bx := blockRasterX(blk)
	by := blockRasterY(blk)
	var nAddr, nx, ny int
	if leftDir {
		nx, ny = bx-1, by
	} else {
		nx, ny = bx, by-1
	}
	nAddr = addr
	if nx < 0 {
		nAddr = d.mbLeft(addr)
		nx = 3
	} else if ny < 0 {
		nAddr = d.mbTop(addr)
		ny = 3
	}
	if nAddr < 0 || !d.mbs[nAddr].available {
		return 2 // DC default
	}
	if d.mbs[nAddr].i16x16 || d.mbs[nAddr].iPCM {
		return 2
	}
	return int(d.mbs[nAddr].i4Modes[blockScanIdx(nx, ny)])
}

// decodeChromaPredMode decodes intra_chroma_pred_mode (TU cMax=3, ctx 64..67).
func (d *decoder) decodeChromaPredMode(addr int) int {
	inc := 0
	if l := d.mbLeft(addr); l >= 0 && d.mbs[l].available && d.mbs[l].chromaMode != 0 {
		inc++
	}
	if t := d.mbTop(addr); t >= 0 && d.mbs[t].available && d.mbs[t].chromaMode != 0 {
		inc++
	}
	if d.c.decodeDecision(ctxIntraChromaPred+inc) == 0 {
		return 0
	}
	mode := 1
	for mode < 3 && d.c.decodeDecision(ctxIntraChromaPred+3) == 1 {
		mode++
	}
	return mode
}

// decodeMBQPDelta decodes mb_qp_delta (ctx 60..63).
func (d *decoder) decodeMBQPDelta(addr int) int {
	inc := 0
	if l := d.mbLeft(addr); l >= 0 && d.mbs[l].available && d.mbs[l].cbp != 0 {
		// prevMbQpDelta != 0 is tracked via a flag; approximate with cbp!=0 of
		// the previous MB in decode order.
	}
	_ = inc
	if d.prevQPDeltaNonZero {
		inc = 1
	}
	if d.c.decodeDecision(ctxMBQPDelta+inc) == 0 {
		d.prevQPDeltaNonZero = false
		return 0
	}
	// unary: value magnitude
	k := 1
	for d.c.decodeDecision(ctxMBQPDelta+2+boolToInt(k > 1)) == 1 {
		k++
		if k > 128 {
			break
		}
	}
	d.prevQPDeltaNonZero = true
	// map unary code index k to signed value: 1->+1? Actually codeNum = k;
	// mb_qp_delta = (-1)^(codeNum+1) * ceil(codeNum/2)
	val := (k + 1) / 2
	if k%2 == 0 {
		val = -val
	}
	return val
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
