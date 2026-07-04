package petkit

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestParseSource(t *testing.T) {
	tests := []struct {
		src       string
		plane     string
		audio     bool
		mediaType uint32
		wantErr   bool
	}{
		{"petkit://main", "main", true, mediaMain | mediaAudio, false},
		{"petkit://main?audio=0", "main", false, mediaMain, false},
		{"petkit://sub", "sub", true, mediaSub | mediaAudio, false},
		{"petkit://sub?audio=0", "sub", false, mediaSub, false},
		{"petkit:main", "main", true, mediaMain | mediaAudio, false},
		{"petkit://", "main", true, mediaMain | mediaAudio, false},
		{"petkit://localhost/sub", "sub", true, mediaSub | mediaAudio, false},
		{"petkit://main.flv", "main", true, mediaMain | mediaAudio, false},
		{"petkit://sub.ts?audio=0", "sub", false, mediaSub, false},
		{"petkit://bogus", "", false, 0, true},
	}

	for _, tt := range tests {
		cfg, err := parseSource(tt.src)
		if tt.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got none", tt.src)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tt.src, err)
			continue
		}
		if cfg.plane != tt.plane || cfg.audio != tt.audio || cfg.mediaType != tt.mediaType {
			t.Errorf("%s: got %+v, want plane=%s audio=%v mediaType=%d",
				tt.src, cfg, tt.plane, tt.audio, tt.mediaType)
		}
	}
}

func TestParseFrameHeader(t *testing.T) {
	h := make([]byte, 0x38)
	binary.LittleEndian.PutUint32(h[0x00:], 42)          // num
	binary.LittleEndian.PutUint32(h[0x08:], 7)           // index
	binary.LittleEndian.PutUint64(h[0x10:], 123_456_789) // pts (us)
	h[0x20] = 1                                           // type: I-frame
	binary.LittleEndian.PutUint16(h[0x22:], mediaMain)   // flags
	binary.LittleEndian.PutUint16(h[0x32:], 24)          // sps len
	binary.LittleEndian.PutUint16(h[0x34:], 6)           // pps len

	f := parseFrameHeader(h)
	if f.Num != 42 || f.Index != 7 || f.PTS != 123_456_789 || f.Type != 1 ||
		f.Flags != mediaMain || f.SPS != 24 || f.PPS != 6 {
		t.Fatalf("parseFrameHeader mismatch: %+v", f)
	}
}

func TestRingReadNoWrap(t *testing.T) {
	ring := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	got := ringRead(ring, 2, 3)
	if !bytes.Equal(got, []byte{2, 3, 4}) {
		t.Fatalf("no-wrap: got %v", got)
	}
}

func TestRingReadWrap(t *testing.T) {
	ring := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	// start near the end, read past the boundary
	got := ringRead(ring, 6, 4)
	if !bytes.Equal(got, []byte{6, 7, 0, 1}) {
		t.Fatalf("wrap: got %v", got)
	}
}

func TestRingReadFullWrap(t *testing.T) {
	ring := []byte{10, 11, 12, 13}
	got := ringRead(ring, 3, 4)
	if !bytes.Equal(got, []byte{13, 10, 11, 12}) {
		t.Fatalf("full-wrap: got %v", got)
	}
}

func TestCstr(t *testing.T) {
	if got := cstr([]byte("ts-server\x00\x00\x00")); got != "ts-server" {
		t.Fatalf("cstr terminated: got %q", got)
	}
	if got := cstr([]byte{'a', 'b', 'c'}); got != "abc" {
		t.Fatalf("cstr unterminated: got %q", got)
	}
}

