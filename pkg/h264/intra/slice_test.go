package intra

import "testing"

// TestParseSliceHeaderI parses the IDR slice header and checks it lands on an
// I-slice with a sane QP and byte-aligned CABAC start.
func TestParseSliceHeaderI(t *testing.T) {
	nals := loadTestNALs(t)
	s, err := parseSPS(findNAL(nals, nalSPS))
	if err != nil {
		t.Fatalf("parseSPS: %v", err)
	}
	p, err := parsePPS(findNAL(nals, nalPPS))
	if err != nil {
		t.Fatalf("parsePPS: %v", err)
	}
	idr := findNAL(nals, nalSliceIDR)
	if idr == nil {
		t.Fatal("no IDR slice")
	}

	// IDR NALs have nal_ref_idc != 0.
	h, r, err := parseSliceHeaderI(idr, s, p, 3)
	if err != nil {
		t.Fatalf("parseSliceHeaderI: %v", err)
	}
	if h.firstMB != 0 {
		t.Errorf("firstMB = %d, want 0", h.firstMB)
	}
	if h.sliceQP < 0 || h.sliceQP > 51 {
		t.Errorf("sliceQP = %d out of range", h.sliceQP)
	}
	if !r.byteAligned() {
		t.Error("CABAC start not byte-aligned")
	}
	// The slice data must have room left for the CABAC engine.
	if r.bitsLeft() < 16 {
		t.Errorf("only %d bits of slice data left", r.bitsLeft())
	}
	t.Logf("firstMB=%d sliceQP=%d disableDeblock=%d bodyByte=%d bitsLeft=%d",
		h.firstMB, h.sliceQP, h.disableDeblock, h.bodyBits/8, r.bitsLeft())
}
