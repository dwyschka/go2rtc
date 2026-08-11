package intra

import (
	"os"
	"testing"
)

// TestDecodeMBTypes validates the CABAC entropy layer end-to-end for the syntax
// pass: it decodes every macroblock of the real IDR and compares the Intra16x16
// vs Intra4x4 decision against ffmpeg's per-MB ground truth (I / i map). A single
// CABAC desync would corrupt every MB after it, so a high match rate proves the
// engine, context tables, and neighbour derivation are correct.
func TestDecodeMBTypes(t *testing.T) {
	annexb, err := os.ReadFile("testdata/idr_main.h264")
	if err != nil {
		t.Fatalf("read idr: %v", err)
	}
	truth, err := os.ReadFile("testdata/mbtype_main.txt")
	if err != nil {
		t.Fatalf("read truth: %v", err)
	}

	d, derr := decodeForTest(annexb)
	decoded := countDecoded(d)
	firstBad := -1
	for i := 0; i < decoded && i < len(truth); i++ {
		if (truth[i] == 'I') != d.mbs[i].i16x16 {
			firstBad = i
			break
		}
	}
	t.Logf("decoded %d/%d MBs, decode err=%v, first mb_type mismatch=%d (row %d col %d)",
		decoded, len(d.mbs), derr, firstBad, firstBad/d.mbW, firstBad%d.mbW)
	if firstBad >= 0 {
		t.Errorf("first mb_type mismatch at MB %d", firstBad)
	} else if decoded < len(d.mbs) {
		t.Errorf("stopped early after %d MBs: %v", decoded, derr)
	}
}

func countDecoded(d *decoder) int {
	n := 0
	for i := range d.mbs {
		if d.mbs[i].available {
			n++
		}
	}
	return n
}
