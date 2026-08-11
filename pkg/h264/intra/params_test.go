package intra

import (
	"os"
	"testing"
)

// loadTestNALs reads the captured IDR access unit and returns its NAL units.
func loadTestNALs(t *testing.T) []nalUnit {
	t.Helper()
	b, err := os.ReadFile("testdata/idr_main.h264")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	nals := splitAnnexB(b)
	if len(nals) < 3 {
		t.Fatalf("expected >=3 NALs (SPS/PPS/IDR), got %d", len(nals))
	}
	return nals
}

func findNAL(nals []nalUnit, typ byte) []byte {
	for _, n := range nals {
		if n.typ == typ {
			return n.rbsp
		}
	}
	return nil
}

// TestParseSPS checks the SPS parse against ffmpeg's trace_headers output for
// the real camera keyframe: Main profile, 108x68 MBs, 1728x1080 cropped.
func TestParseSPS(t *testing.T) {
	nals := loadTestNALs(t)
	rbsp := findNAL(nals, nalSPS)
	if rbsp == nil {
		t.Fatal("no SPS NAL")
	}
	s, err := parseSPS(rbsp)
	if err != nil {
		t.Fatalf("parseSPS: %v", err)
	}

	if s.profileIDC != 77 {
		t.Errorf("profileIDC = %d, want 77 (Main)", s.profileIDC)
	}
	if s.levelIDC != 51 {
		t.Errorf("levelIDC = %d, want 51", s.levelIDC)
	}
	if s.widthMbs != 108 || s.heightMapUnits != 68 {
		t.Errorf("dims = %dx%d MBs, want 108x68", s.widthMbs, s.heightMapUnits)
	}
	if !s.frameMbsOnly {
		t.Error("frameMbsOnly = false, want true")
	}
	if s.cropBottom != 4 {
		t.Errorf("cropBottom = %d, want 4", s.cropBottom)
	}
	if s.width() != 1728 || s.height() != 1080 {
		t.Errorf("cropped = %dx%d, want 1728x1080", s.width(), s.height())
	}
	if s.codedWidth() != 1728 || s.codedHeight() != 1088 {
		t.Errorf("coded = %dx%d, want 1728x1088", s.codedWidth(), s.codedHeight())
	}
}

// TestParsePPS checks the PPS parse: CABAC, single slice group, unconstrained
// intra.
func TestParsePPS(t *testing.T) {
	nals := loadTestNALs(t)
	rbsp := findNAL(nals, nalPPS)
	if rbsp == nil {
		t.Fatal("no PPS NAL")
	}
	p, err := parsePPS(rbsp)
	if err != nil {
		t.Fatalf("parsePPS: %v", err)
	}

	if !p.entropyCABAC {
		t.Error("entropyCABAC = false, want true")
	}
	if p.numSliceGroups != 1 {
		t.Errorf("numSliceGroups = %d, want 1", p.numSliceGroups)
	}
	if p.constrainedIntra {
		t.Error("constrainedIntra = true, want false")
	}
	if p.transform8x8 {
		t.Error("transform8x8 = true, want false (Main profile)")
	}
	// pic_init_qp is 26 + minus26; just sanity-check it is in range.
	qp := 26 + p.picInitQPMinus26
	if qp < 0 || qp > 51 {
		t.Errorf("pic_init_qp = %d, out of range", qp)
	}
	t.Logf("pic_init_qp=%d chroma_qp_offset=%d deblockCtrl=%v", qp, p.chromaQPIndexOffset, p.deblockingCtrl)
}

// TestFindIDR confirms the IDR slice NAL is present.
func TestFindIDR(t *testing.T) {
	nals := loadTestNALs(t)
	if findNAL(nals, nalSliceIDR) == nil {
		t.Fatal("no IDR slice NAL")
	}
}
