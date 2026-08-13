package intra

import "errors"

var (
	errUnsupported = errors.New("intra: unsupported H.264 feature")
	errNoIDR       = errors.New("intra: no IDR slice found")
	errNoParams    = errors.New("intra: missing SPS/PPS")
)

// sps holds the sequence parameters this decoder needs. Fields it parses but
// does not use (POC, ref frames) are consumed to stay bit-aligned.
type sps struct {
	profileIDC      uint32
	levelIDC        uint32
	id              uint32
	chromaFormatIDC uint32 // 1 = 4:2:0 (only value supported)
	widthMbs        uint32 // pic_width_in_mbs_minus1 + 1
	heightMapUnits  uint32 // pic_height_in_map_units_minus1 + 1
	frameMbsOnly    bool
	cropLeft        uint32
	cropRight       uint32
	cropTop         uint32
	cropBottom      uint32

	log2MaxFrameNum uint32
	picOrderCntType uint32
	log2MaxPOCLsb   uint32
}

// coded luma dimensions in pixels (macroblock-aligned, before cropping).
func (s *sps) codedWidth() int  { return int(s.widthMbs) * 16 }
func (s *sps) codedHeight() int { return int(s.frameHeightMbs()) * 16 }

func (s *sps) frameHeightMbs() uint32 {
	if s.frameMbsOnly {
		return s.heightMapUnits
	}
	return s.heightMapUnits * 2
}

// cropped (display) dimensions in pixels, for 4:2:0 progressive.
func (s *sps) width() int {
	return s.codedWidth() - int(2*(s.cropLeft+s.cropRight))
}

func (s *sps) height() int {
	cropUnitY := uint32(2)
	if !s.frameMbsOnly {
		cropUnitY = 4
	}
	return s.codedHeight() - int(cropUnitY*(s.cropTop+s.cropBottom))
}

func parseSPS(rbsp []byte) (*sps, error) {
	r := newBitReader(rbsp)
	s := &sps{chromaFormatIDC: 1}
	s.profileIDC = r.readBits(8)
	_ = r.readBits(8) // constraint_set flags + reserved
	s.levelIDC = r.readBits(8)
	s.id = r.readUE()

	switch s.profileIDC {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		// High-profile family carries chroma/bit-depth/scaling info. We only
		// need to skip past it to stay aligned; this decoder targets Main.
		s.chromaFormatIDC = r.readUE()
		if s.chromaFormatIDC == 3 {
			r.readFlag() // separate_colour_plane_flag
		}
		r.readUE() // bit_depth_luma_minus8
		r.readUE() // bit_depth_chroma_minus8
		r.readFlag()
		if r.readFlag() { // seq_scaling_matrix_present_flag
			n := 8
			if s.chromaFormatIDC == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.readFlag() {
					skipScalingList(r, boolTo(i < 6, 16, 64))
				}
			}
		}
	}

	s.log2MaxFrameNum = r.readUE() + 4
	s.picOrderCntType = r.readUE()
	switch s.picOrderCntType {
	case 0:
		s.log2MaxPOCLsb = r.readUE() + 4
	case 1:
		r.readFlag() // delta_pic_order_always_zero_flag
		r.readSE()   // offset_for_non_ref_pic
		r.readSE()   // offset_for_top_to_bottom_field
		n := r.readUE()
		for i := uint32(0); i < n; i++ {
			r.readSE()
		}
	}

	r.readUE()             // max_num_ref_frames
	r.readFlag()           // gaps_in_frame_num_value_allowed_flag
	s.widthMbs = r.readUE() + 1
	s.heightMapUnits = r.readUE() + 1
	s.frameMbsOnly = r.readFlag()
	if !s.frameMbsOnly {
		r.readFlag() // mb_adaptive_frame_field_flag
	}
	r.readFlag() // direct_8x8_inference_flag
	if r.readFlag() {
		s.cropLeft = r.readUE()
		s.cropRight = r.readUE()
		s.cropTop = r.readUE()
		s.cropBottom = r.readUE()
	}
	// vui_parameters follow but are not needed for decoding.

	if s.chromaFormatIDC != 1 {
		return nil, errUnsupported // only 4:2:0
	}
	return s, nil
}

// pps holds the picture parameters this decoder needs.
type pps struct {
	id                  uint32
	spsID               uint32
	entropyCABAC        bool
	numSliceGroups      uint32
	picInitQPMinus26    int
	chromaQPIndexOffset int
	deblockingCtrl      bool
	constrainedIntra    bool
	transform8x8        bool
}

func parsePPS(rbsp []byte) (*pps, error) {
	r := newBitReader(rbsp)
	p := &pps{}
	p.id = r.readUE()
	p.spsID = r.readUE()
	p.entropyCABAC = r.readFlag()
	r.readFlag() // bottom_field_pic_order_in_frame_present_flag
	p.numSliceGroups = r.readUE() + 1
	if p.numSliceGroups > 1 {
		return nil, errUnsupported // no FMO
	}
	r.readUE()   // num_ref_idx_l0_default_active_minus1
	r.readUE()   // num_ref_idx_l1_default_active_minus1
	r.readFlag() // weighted_pred_flag
	r.readBits(2)
	p.picInitQPMinus26 = r.readSE()
	r.readSE() // pic_init_qs_minus26
	p.chromaQPIndexOffset = r.readSE()
	p.deblockingCtrl = r.readFlag()
	p.constrainedIntra = r.readFlag()
	r.readFlag() // redundant_pic_cnt_present_flag

	if r.moreRBSPData() {
		p.transform8x8 = r.readFlag()
		if r.readFlag() { // pic_scaling_matrix_present_flag
			return nil, errUnsupported // scaling lists not supported
		}
		r.readSE() // second_chroma_qp_index_offset
	}
	return p, nil
}

// skipScalingList consumes a scaling list without applying it.
func skipScalingList(r *bitReader, size int) {
	last, next := 8, 8
	for j := 0; j < size; j++ {
		if next != 0 {
			next = (last + r.readSE() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

func boolTo(c bool, t, f int) int {
	if c {
		return t
	}
	return f
}
