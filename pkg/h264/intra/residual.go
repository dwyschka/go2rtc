package intra

// residualBlock decodes one residual block's CABAC syntax (9.3.3.1.3 /
// residual_block_cabac) into coeffLevel (in scan order, length maxNumCoeff).
// cat is the ctxBlockCat (0..4), cbfCtxInc the coded_block_flag context
// increment derived from neighbours. Returns whether the block had any nonzero
// coefficient (coded_block_flag).
//
// If hasCBF is false the coded_block_flag bin is not decoded and the block is
// assumed present (used for the Intra16x16 DC / when CBP already implies it? —
// here we always decode coded_block_flag, matching CABAC).
func (d *decoder) residualBlock(cat int, cbfCtxInc int, coeffLevel []int32) bool {
	maxNumCoeff := catMaxCoeff[cat]

	cbf := d.c.decodeDecision(ctxCodedBlockFlag + cbfCatOffset[cat] + cbfCtxInc)
	if cbf == 0 {
		return false
	}

	// significant_coeff_flag / last_significant_coeff_flag
	sig := make([]bool, maxNumCoeff)
	numSig := 0
	i := 0
	for ; i < maxNumCoeff-1; i++ {
		inc := sigCtxInc(cat, i)
		if d.c.decodeDecision(ctxSigCoeffFlag+sigCatOffset[cat]+inc) == 1 {
			sig[i] = true
			numSig++
			linc := sigCtxInc(cat, i) // last uses the same increment map for 4x4
			if d.c.decodeDecision(ctxLastSigCoeff+lastCatOffset[cat]+linc) == 1 {
				i++
				goto levels
			}
		}
	}
	// The final scan position is significant if we fell through.
	sig[maxNumCoeff-1] = true
	numSig++
	i = maxNumCoeff

levels:
	_ = i
	// coeff_abs_level_minus1 + sign, decoded from the last significant coeff
	// backwards (9.3.3.1.1.10).
	numEq1 := 0
	numGt1 := 0
	for j := maxNumCoeff - 1; j >= 0; j-- {
		if !sig[j] {
			continue
		}
		var ctxInc int
		if numGt1 != 0 {
			ctxInc = 0
		} else {
			ctxInc = 1 + numEq1
			if ctxInc > 4 {
				ctxInc = 4
			}
		}
		absLevel := d.decodeAbsLevelMinus1(cat, ctxInc, numGt1) + 1
		sign := d.c.decodeBypass()
		level := int32(absLevel)
		if sign == 1 {
			level = -level
		}
		coeffLevel[j] = level
		if absLevel == 1 {
			numEq1++
		} else {
			numGt1++
		}
	}
	return true
}

// decodeAbsLevelMinus1 decodes coeff_abs_level_minus1 (UEG0, uCoff=14). ctxInc0
// is the binIdx-0 context increment; subsequent prefix bins use 5+min(4,numGt1).
func (d *decoder) decodeAbsLevelMinus1(cat, ctxInc0, numGt1 int) int {
	base := ctxCoeffAbsLevel + absLevelCatOffset[cat]

	// prefix: truncated unary, cMax = 14
	if d.c.decodeDecision(base+ctxInc0) == 0 {
		return 0
	}
	inc := 5 + numGt1
	if inc > 5+4 {
		inc = 5 + 4
	}
	prefix := 1
	for prefix < 14 {
		if d.c.decodeDecision(base+inc) == 0 {
			return prefix
		}
		prefix++
	}
	// prefix == 14 -> suffix is Exp-Golomb order 0 in bypass (EG0)
	suffix := 0
	k := 0
	for d.c.decodeBypass() == 1 {
		suffix += 1 << uint(k)
		k++
		if k > 30 {
			break
		}
	}
	for k > 0 {
		k--
		suffix += int(d.c.decodeBypass()) << uint(k)
	}
	return 14 + suffix
}

// sigCtxInc returns the significant/last coeff context increment for scan
// position i (9.3.3.1.3). For 4x4 blocks it is the scan position; for chroma DC
// (cat 3, 4:2:0) it is min(i, 2).
func sigCtxInc(cat, i int) int {
	if cat == 3 { // chroma DC (4:2:0, NumC8x8 == 1)
		if i > 2 {
			return 2
		}
		return i
	}
	return i
}
