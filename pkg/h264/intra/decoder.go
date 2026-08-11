package intra

import (
	"image"
)

// Decode decodes the first IDR keyframe found in an Annex-B H.264 bitstream and
// returns it as an image (YCbCr 4:2:0). Only Main-profile, 4:2:0, 4x4-transform,
// progressive, single-slice-group I-frames are supported — exactly what the
// Petkit AXERA cameras emit. Any unsupported feature returns an error.
func Decode(annexb []byte) (image.Image, error) {
	nals := splitAnnexB(annexb)

	var spsRB, ppsRB, idrRB []byte
	for _, n := range nals {
		switch n.typ {
		case nalSPS:
			if spsRB == nil {
				spsRB = n.rbsp
			}
		case nalPPS:
			if ppsRB == nil {
				ppsRB = n.rbsp
			}
		case nalSliceIDR:
			if idrRB == nil {
				idrRB = n.rbsp
			}
		}
	}
	if spsRB == nil || ppsRB == nil {
		return nil, errNoParams
	}
	if idrRB == nil {
		return nil, errNoIDR
	}

	s, err := parseSPS(spsRB)
	if err != nil {
		return nil, err
	}
	p, err := parsePPS(ppsRB)
	if err != nil {
		return nil, err
	}
	if !p.entropyCABAC {
		return nil, errUnsupported // this decoder is CABAC-only for now
	}

	d := newDecoder(s, p)
	if err := d.decodeSliceIDR(idrRB); err != nil {
		return nil, err
	}
	return d.toImage(), nil
}

// decodeForTest parses the IDR and returns the decoder (with per-MB state) for
// validation against ffmpeg ground truth. Not part of the public API.
func decodeForTest(annexb []byte) (*decoder, error) {
	nals := splitAnnexB(annexb)
	var spsRB, ppsRB, idrRB []byte
	for _, n := range nals {
		switch n.typ {
		case nalSPS:
			spsRB = n.rbsp
		case nalPPS:
			ppsRB = n.rbsp
		case nalSliceIDR:
			idrRB = n.rbsp
		}
	}
	s, err := parseSPS(spsRB)
	if err != nil {
		return nil, err
	}
	p, err := parsePPS(ppsRB)
	if err != nil {
		return nil, err
	}
	d := newDecoder(s, p)
	if err := d.decodeSliceIDR(idrRB); err != nil {
		return d, err
	}
	return d, nil
}

// mbInfo holds the per-macroblock state needed for neighbour-based context
// derivation and reconstruction.
type mbInfo struct {
	available bool
	iPCM      bool
	i16x16    bool  // Intra_16x16 (vs Intra_4x4)
	qp        int   // luma QP used for this MB
	cbp       int   // coded_block_pattern
	i4Modes   [16]int8 // per-4x4 Intra4x4PredMode (in raster-of-scan order)
	i16Mode   int   // Intra16x16PredMode
	chromaMode int  // intra_chroma_pred_mode
	// coded_block_flag per block, for cbf context of neighbours:
	//   [0]=luma DC, [1..16]=luma AC/4x4 (raster 4x4 idx), [17]=cb DC, [18]=cr DC,
	//   [19..22]=cb AC, [23..26]=cr AC
	cbf [27]uint8
}

// decoder holds the whole picture-decode state.
type decoder struct {
	sps *sps
	pps *pps
	c   cabac

	mbW, mbH int // picture size in macroblocks
	mbs      []mbInfo

	// reconstructed planes (coded, macroblock-aligned size).
	y, cb, cr []byte
	strideY   int
	strideC   int

	qp                 int  // running QP (QPy)
	prevQPDeltaNonZero bool // for mb_qp_delta context

	// per-picture coded_block_flag grids for residual context derivation.
	// luma: mbW*4 by mbH*4 (one per 4x4 block); chroma: mbW*2 by mbH*2 per plane.
	cbfLuma   []uint8
	cbfCb     []uint8
	cbfCr     []uint8
	cbfLumaDC []uint8 // per MB (I_16x16 luma DC)
	cbfCbDC   []uint8
	cbfCrDC   []uint8
}

func newDecoder(s *sps, p *pps) *decoder {
	d := &decoder{sps: s, pps: p}
	d.mbW = int(s.widthMbs)
	d.mbH = int(s.frameHeightMbs())
	d.mbs = make([]mbInfo, d.mbW*d.mbH)
	d.strideY = d.mbW * 16
	d.strideC = d.mbW * 8
	d.y = make([]byte, d.strideY*d.mbH*16)
	d.cb = make([]byte, d.strideC*d.mbH*8)
	d.cr = make([]byte, d.strideC*d.mbH*8)
	d.cbfLuma = make([]uint8, d.mbW*4*d.mbH*4)
	d.cbfCb = make([]uint8, d.mbW*2*d.mbH*2)
	d.cbfCr = make([]uint8, d.mbW*2*d.mbH*2)
	d.cbfLumaDC = make([]uint8, d.mbW*d.mbH)
	d.cbfCbDC = make([]uint8, d.mbW*d.mbH)
	d.cbfCrDC = make([]uint8, d.mbW*d.mbH)
	return d
}

// mbAt returns the mbInfo at macroblock (mbx,mby) or a zero (unavailable) one.
func (d *decoder) mbAddr(mbx, mby int) int { return mby*d.mbW + mbx }

// zigZag4x4 maps scan position -> raster index in a 4x4 block (Fig 8-8, frame).
var zigZag4x4 = [16]int{0, 1, 4, 8, 5, 2, 3, 6, 9, 12, 13, 10, 7, 11, 14, 15}

// ctxBlockCat offsets for coded_block_flag / significant / last / abs_level
// (Tables 9-40). Index by ctxBlockCat 0..4.
var (
	cbfCatOffset      = [5]int{0, 4, 8, 12, 16}
	sigCatOffset      = [5]int{0, 15, 29, 44, 47}
	lastCatOffset     = [5]int{0, 15, 29, 44, 47}
	absLevelCatOffset = [5]int{0, 10, 20, 30, 39}
)

// maxNumCoeff per ctxBlockCat (4:2:0).
var catMaxCoeff = [5]int{16, 15, 16, 4, 15}

// toImage crops the reconstructed planes to display size and returns a YCbCr.
func (d *decoder) toImage() image.Image {
	w, h := d.sps.width(), d.sps.height()
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for y := 0; y < h; y++ {
		copy(img.Y[y*img.YStride:y*img.YStride+w], d.y[y*d.strideY:y*d.strideY+w])
	}
	cw, ch := (w+1)/2, (h+1)/2
	for y := 0; y < ch; y++ {
		copy(img.Cb[y*img.CStride:y*img.CStride+cw], d.cb[y*d.strideC:y*d.strideC+cw])
		copy(img.Cr[y*img.CStride:y*img.CStride+cw], d.cr[y*d.strideC:y*d.strideC+cw])
	}
	return img
}
