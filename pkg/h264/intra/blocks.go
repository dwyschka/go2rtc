package intra

// 4x4 luma block index (decode order 0..15) <-> raster position within the MB
// (bx,by in 4x4 units). The 16 blocks scan as four 8x8 blocks in raster, each of
// four 4x4 in raster (6.4.3).
func blockRasterX(blk int) int { return (blk/4&1)*2 + (blk%4)&1 }
func blockRasterY(blk int) int { return (blk/4>>1)*2 + (blk%4)>>1 }
func blockScanIdx(bx, by int) int {
	return (by/2*2+bx/2)*4 + (by%2)*2 + bx%2
}

// picture 4x4 luma block coordinate for MB addr, block blk.
func (d *decoder) lumaBlockXY(addr, blk int) (int, int) {
	mbx, mby := addr%d.mbW, addr/d.mbW
	return mbx*4 + blockRasterX(blk), mby*4 + blockRasterY(blk)
}

// cbfCtxLuma computes the coded_block_flag ctxIdxInc for a luma 4x4/AC block from
// the left+top neighbours (9.3.3.1.1.9). All macroblocks here are intra, so an
// unavailable (picture-edge) neighbour contributes condTermFlag 1.
func (d *decoder) cbfCtxLuma(px, py int) int {
	w := d.mbW * 4
	a := 1
	if px > 0 {
		a = int(d.cbfLuma[py*w+px-1])
	}
	b := 1
	if py > 0 {
		b = int(d.cbfLuma[(py-1)*w+px])
	}
	return a + 2*b
}

func (d *decoder) cbfCtxChroma(grid []uint8, px, py int) int {
	w := d.mbW * 2
	a := 1
	if px > 0 {
		a = int(grid[py*w+px-1])
	}
	b := 1
	if py > 0 {
		b = int(grid[(py-1)*w+px])
	}
	return a + 2*b
}

// cbfCtxDC computes the coded_block_flag ctx for a DC block (luma I_16x16 DC or
// chroma DC) from the neighbouring MBs' DC cbf (per-MB grid).
func (d *decoder) cbfCtxDC(grid []uint8, addr int) int {
	a := 1
	if l := d.mbLeft(addr); l >= 0 {
		a = int(grid[l])
	}
	b := 1
	if t := d.mbTop(addr); t >= 0 {
		b = int(grid[t])
	}
	return a + 2*b
}

// decodeCBP decodes coded_block_pattern for an I_NxN MB (9.3.2.6). Luma is 4
// bins (one per 8x8) with neighbour context; chroma is a prefix/suffix.
func (d *decoder) decodeCBP(addr int) int {
	mbx, mby := addr%d.mbW, addr/d.mbW
	cbpLuma := 0
	for i8 := 0; i8 < 4; i8++ {
		bx, by := i8&1, i8>>1 // 8x8 position within MB
		// neighbour 8x8 cbp bits (condTermFlag = 1 if neighbour block has cbp 0).
		condA := d.cbp8x8(mbx, mby, bx-1, by, cbpLuma, addr)
		condB := d.cbp8x8(mbx, mby, bx, by-1, cbpLuma, addr)
		inc := condA + 2*condB
		if d.c.decodeDecision(ctxCBPLuma+inc) == 1 {
			cbpLuma |= 1 << uint(i8)
		}
	}
	// chroma: ctxIdxInc from neighbour chroma cbp (9.3.3.1.1.4).
	cInc := d.cbpChromaCtx(addr, 0)
	cbpChroma := 0
	if d.c.decodeDecision(ctxCBPChroma+cInc) == 1 {
		cInc2 := d.cbpChromaCtx(addr, 1)
		if d.c.decodeDecision(ctxCBPChroma+4+cInc2) == 1 {
			cbpChroma = 2
		} else {
			cbpChroma = 1
		}
	}
	return cbpLuma | (cbpChroma << 4)
}

// cbp8x8 returns condTermFlag (1 if the neighbour 8x8 luma block is available and
// has a zero cbp bit, else 0) for the luma cbp context. curCbpLuma holds the
// already-decoded bits of the current MB.
func (d *decoder) cbp8x8(mbx, mby, bx, by, curCbpLuma, addr int) int {
	if bx >= 0 && by >= 0 {
		// same MB, already decoded
		bit := (curCbpLuma >> uint(by*2+bx)) & 1
		return 1 - bit
	}
	var nAddr, nbx, nby int
	if bx < 0 {
		nAddr, nbx, nby = d.mbLeft(addr), 1, by
	} else {
		nAddr, nbx, nby = d.mbTop(addr), bx, 1
	}
	if nAddr < 0 || !d.mbs[nAddr].available {
		return 0 // unavailable -> condTermFlag 0 for luma cbp
	}
	if d.mbs[nAddr].iPCM {
		return 0
	}
	bit := (d.mbs[nAddr].cbp >> uint(nby*2+nbx)) & 1
	return 1 - bit
}

// cbpChromaCtx returns the ctxIdxInc for the chroma cbp bins (binIdx 0 or 1).
func (d *decoder) cbpChromaCtx(addr, binIdx int) int {
	cond := func(nAddr int) int {
		if nAddr < 0 || !d.mbs[nAddr].available {
			return 0
		}
		if d.mbs[nAddr].iPCM {
			return 1
		}
		c := d.mbs[nAddr].cbp >> 4
		if binIdx == 0 {
			if c != 0 {
				return 1
			}
			return 0
		}
		if c == 2 {
			return 1
		}
		return 0
	}
	return cond(d.mbLeft(addr)) + 2*cond(d.mbTop(addr))
}

// decodeResiduals decodes all residual blocks of the MB, storing cbf in the
// per-picture grids for neighbour context.
func (d *decoder) decodeResiduals(addr, cbpLuma, cbpChroma int) error {
	mb := &d.mbs[addr]
	w4 := d.mbW * 4
	w2 := d.mbW * 2

	if mb.i16x16 {
		// luma DC (cat 0)
		var dc [16]int32
		if d.residualBlock(0, d.cbfCtxDC(d.cbfLumaDC, addr), dc[:]) {
			d.cbfLumaDC[addr] = 1
		}
		// luma AC (cat 1) per 4x4 block, if cbpLuma
		for blk := 0; blk < 16; blk++ {
			px, py := d.lumaBlockXY(addr, blk)
			if cbpLuma != 0 {
				var ac [15]int32
				if d.residualBlock(1, d.cbfCtxLuma(px, py), ac[:]) {
					d.cbfLuma[py*w4+px] = 1
				}
			}
		}
	} else {
		// I_NxN: 16 luma 4x4 (cat 2), grouped by cbpLuma 8x8 bits
		for blk := 0; blk < 16; blk++ {
			px, py := d.lumaBlockXY(addr, blk)
			i8 := (blockRasterY(blk)/2)*2 + blockRasterX(blk)/2
			if (cbpLuma>>uint(i8))&1 != 0 {
				var lv [16]int32
				if d.residualBlock(2, d.cbfCtxLuma(px, py), lv[:]) {
					d.cbfLuma[py*w4+px] = 1
				}
			}
		}
	}

	// chroma
	if cbpChroma != 0 {
		// chroma DC (cat 3), 2 planes
		var cbDC, crDC [4]int32
		if d.residualBlock(3, d.cbfCtxDC(d.cbfCbDC, addr), cbDC[:]) {
			d.cbfCbDC[addr] = 1
		}
		if d.residualBlock(3, d.cbfCtxDC(d.cbfCrDC, addr), crDC[:]) {
			d.cbfCrDC[addr] = 1
		}
		if cbpChroma == 2 {
			// chroma AC (cat 4), 4 blocks per plane
			mbx, mby := addr%d.mbW, addr/d.mbW
			for plane, grid := range [][]uint8{d.cbfCb, d.cbfCr} {
				for blk := 0; blk < 4; blk++ {
					px := mbx*2 + blk&1
					py := mby*2 + blk>>1
					var ac [15]int32
					if d.residualBlock(4, d.cbfCtxChroma(grid, px, py), ac[:]) {
						grid[py*w2+px] = 1
					}
					_ = plane
				}
			}
		}
	}
	return nil
}
