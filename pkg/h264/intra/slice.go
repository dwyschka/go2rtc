package intra

// sliceHeader holds the fields of an IDR I-slice header we need to start
// macroblock decoding. Only I-slices are supported.
type sliceHeader struct {
	firstMB  uint32
	sliceQP  int  // 26 + pic_init_qp_minus26 + slice_qp_delta
	disableDeblock uint32
	alphaC0Off int
	betaOff    int
	bodyBits   int // bit position where the slice data (CABAC) begins
}

// isISlice reports whether a slice_type value denotes an I-slice (2 or 7).
func isISlice(t uint32) bool { return t == 2 || t == 7 }

// parseSliceHeaderI parses an IDR I-slice header from the slice RBSP. It returns
// the header plus leaves the bit reader positioned at the start of the CABAC
// slice data (after cabac_alignment_one_bit padding). nalRefIdc is the NAL
// header's ref-idc (nonzero for IDR).
func parseSliceHeaderI(rbsp []byte, s *sps, p *pps, nalRefIdc byte) (*sliceHeader, *bitReader, error) {
	r := newBitReader(rbsp)
	h := &sliceHeader{}

	h.firstMB = r.readUE()
	sliceType := r.readUE()
	if !isISlice(sliceType) {
		return nil, nil, errUnsupported // only I-slices
	}
	r.readUE() // pic_parameter_set_id (assumed to match p)

	r.readBits(int(s.log2MaxFrameNum)) // frame_num
	// frame_mbs_only_flag == 1 -> no field_pic_flag

	// IDR: this NAL type carries idr_pic_id. The caller only invokes this for
	// nalSliceIDR, so idr_pic_id is always present.
	r.readUE() // idr_pic_id

	if s.picOrderCntType == 0 {
		r.readBits(int(s.log2MaxPOCLsb)) // pic_order_cnt_lsb
		// bottom_field_pic_order_in_frame_present is off in this stream.
	}

	// I-slice: no direct_spatial_mv_pred, no num_ref_idx override, no
	// ref_pic_list_modification, no pred_weight_table.

	// dec_ref_pic_marking for IDR (nal_ref_idc != 0).
	if nalRefIdc != 0 {
		r.readFlag() // no_output_of_prior_pics_flag
		r.readFlag() // long_term_reference_flag
	}

	// cabac_init_idc is only present for non-I slices, so skip it here.

	sliceQPDelta := r.readSE()
	h.sliceQP = 26 + p.picInitQPMinus26 + sliceQPDelta
	if h.sliceQP < 0 || h.sliceQP > 51 {
		return nil, nil, errUnsupported
	}

	if p.deblockingCtrl {
		h.disableDeblock = r.readUE()
		if h.disableDeblock != 1 {
			h.alphaC0Off = r.readSE() * 2
			h.betaOff = r.readSE() * 2
		}
	}

	// CABAC: align to a byte boundary via cabac_alignment_one_bit (all 1s).
	if p.entropyCABAC {
		for !r.byteAligned() {
			r.readBit()
		}
	}
	h.bodyBits = r.pos
	return h, r, nil
}
