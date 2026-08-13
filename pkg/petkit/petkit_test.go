package petkit

import (
	"bytes"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/aac"
)

func TestAACSilenceFrameIsValidADTS(t *testing.T) {
	enc := newAACEncoder(16000, 1)
	frame := enc.EncodeFrame(make([]int16, aacFrameSamples))

	if !aac.IsADTS(frame) {
		t.Fatalf("not a valid ADTS frame: % x", frame[:min(7, len(frame))])
	}
	if got := int(aac.ReadADTSSize(frame)); got != len(frame) {
		t.Fatalf("ADTS frame_length %d != actual %d", got, len(frame))
	}
	// 16 kHz mono AAC-LC -> the decoder must recognise the codec.
	if c := aac.ADTSToCodec(frame); c == nil || c.ClockRate != 16000 || c.Channels != 1 {
		t.Fatalf("bad codec from ADTS: %+v", c)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

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

// buildFrame writes a descriptor with the given values using a layout's own
// offsets, so a round-trip through parseFrame proves the offsets are consistent.
func buildFrame(l frameLayout, num, index uint32, pts uint64, typ uint8, flags, sps, pps uint16) []byte {
	h := make([]byte, l.hdrSize)
	putU32(h, l.offNum, num)
	putU32(h, l.offIndex, index)
	putU64(h, l.offPTS, pts)
	putByte(h, l.offType, typ)
	putU16(h, l.offFlags, flags)
	putU16(h, l.offSPS, sps)
	putU16(h, l.offPPS, pps)
	return h
}

func TestParseFrameHeader(t *testing.T) {
	for _, l := range []frameLayout{layoutARM, layoutT7, layoutW7H} {
		t.Run(l.name, func(t *testing.T) {
			h := buildFrame(l, 42, 7, 123_456_789, 1, mediaMain, 24, 6)
			f := l.parseFrame(h)
			if f.Num != 42 || f.Index != 7 || f.PTS != 123_456_789 || f.Type != 1 ||
				f.Flags != mediaMain || f.SPS != 24 || f.PPS != 6 {
				t.Fatalf("parseFrame mismatch: %+v", f)
			}
		})
	}
}

// TestLayoutOffsetsMatchRE pins the reverse-engineered offsets so a future edit
// that silently shifts them fails loudly. ARM comes from agora __on_audio_data;
// T7 from t7_libbase.so mbuffer_read_frame/mbuffer_write_frame + avget_get_sample.
func TestLayoutOffsetsMatchRE(t *testing.T) {
	if layoutARM.hdrSize != 0x38 || layoutARM.offType != 0x20 || layoutARM.offFlags != 0x22 {
		t.Errorf("ARM layout drifted: %+v", layoutARM)
	}
	if layoutT7.hdrSize != 0x35 || layoutT7.offType != 0x18 || layoutT7.offFlags != 0x1a ||
		layoutT7.offSPS != 0x2f || layoutT7.offPPS != 0x31 {
		t.Errorf("T7 layout drifted: %+v", layoutT7)
	}
	// The Ingenic-T7 descriptor has no codec/sample/capture fields.
	if layoutT7.offCodec != -1 || layoutT7.offSamples != -1 || layoutT7.offCaptureUS != -1 {
		t.Errorf("T7 layout should not carry ARM-only write fields: %+v", layoutT7)
	}
	// w7h (ARM v8): read side from tserver_w7h (0x3D descriptor, ARM-identical
	// through type_flags@0x22, whole tail shifted +5 -> SPS/PPS @0x37/0x39);
	// write side pinned from agora_w7h __on_audio_data (codec@0x21, and the AAC
	// sample hints samples@0x33 / khz@0x35, also +5 from ARM's 0x2e/0x30). Has a
	// speaker -> talkback.
	if layoutW7H.hdrSize != 0x3d || layoutW7H.offType != 0x20 || layoutW7H.offFlags != 0x22 ||
		layoutW7H.offSPS != 0x37 || layoutW7H.offPPS != 0x39 {
		t.Errorf("w7h layout drifted: %+v", layoutW7H)
	}
	if !layoutW7H.speaker || layoutW7H.offCodec != 0x21 || layoutW7H.offCaptureUS != 0x18 ||
		layoutW7H.offWallSec != 0x0c {
		t.Errorf("w7h layout should be speaker-capable with ARM-front write fields: %+v", layoutW7H)
	}
	if layoutW7H.offSamples != 0x33 || layoutW7H.offKHz != 0x35 {
		t.Errorf("w7h AAC sample hints must match agora_w7h __on_audio_data (+5 from ARM): %+v", layoutW7H)
	}
}

func TestSelectLayout(t *testing.T) {
	cases := map[string]string{
		"":        "arm", // default
		"arm":     "arm",
		"AXERA":   "arm",
		"d4sh":    "arm",
		"d4sh2":   "arm", // byte-identical descriptor (verified tserver_d4sh2)
		"t7":      "t7",
		"mips":    "t7",
		"Ingenic": "t7",
		"w7h":     "w7h",
		"W7H":     "w7h",
		"w7":      "w7h",
	}
	for in, want := range cases {
		l, err := selectLayout(in)
		if err != nil {
			t.Errorf("selectLayout(%q): unexpected error %v", in, err)
			continue
		}
		if l.name != want {
			t.Errorf("selectLayout(%q) = %s, want %s", in, l.name, want)
		}
	}
	if _, err := selectLayout("nonsense"); err == nil {
		t.Errorf("selectLayout(nonsense) should error")
	}
}

func TestResolveLayoutEnv(t *testing.T) {
	t.Setenv("PETKIT_LAYOUT", "t7")
	if l, err := resolveLayout(""); err != nil || l.name != "t7" {
		t.Fatalf("env fallback: got %s, %v", l.name, err)
	}
	// An explicit value wins over the env var.
	if l, err := resolveLayout("arm"); err != nil || l.name != "arm" {
		t.Fatalf("explicit override: got %s, %v", l.name, err)
	}
}

func TestParseSourceLayout(t *testing.T) {
	if cfg, err := parseSource("petkit://main"); err != nil || cfg.layout.name != "arm" {
		t.Fatalf("default layout: got %s, %v", cfg.layout.name, err)
	}
	if cfg, err := parseSource("petkit://main?layout=t7"); err != nil || cfg.layout.name != "t7" {
		t.Fatalf("query layout: got %s, %v", cfg.layout.name, err)
	}
	if cfg, err := parseSource("petkit://sub?audio=0&layout=mips"); err != nil ||
		cfg.layout.name != "t7" || cfg.mediaType != mediaSub {
		t.Fatalf("query layout+audio: got %+v, %v", cfg, err)
	}
	if _, err := parseSource("petkit://main?layout=bogus"); err == nil {
		t.Fatalf("bogus layout should error")
	}
}

func TestParseSourceTalkback(t *testing.T) {
	// ARM has a speaker -> talkback advertised by default.
	if cfg, err := parseSource("petkit://main"); err != nil || !cfg.talkback {
		t.Fatalf("arm default talkback: got %v, %v", cfg.talkback, err)
	}
	// T7 is mic-only -> no talkback by default.
	if cfg, err := parseSource("petkit://main?layout=t7"); err != nil || cfg.talkback {
		t.Fatalf("t7 default talkback: got %v, %v", cfg.talkback, err)
	}
	// Explicit override both ways.
	if cfg, err := parseSource("petkit://main?layout=t7&talkback=1"); err != nil || !cfg.talkback {
		t.Fatalf("t7 talkback=1: got %v, %v", cfg.talkback, err)
	}
	if cfg, err := parseSource("petkit://main?talkback=0"); err != nil || cfg.talkback {
		t.Fatalf("arm talkback=0: got %v, %v", cfg.talkback, err)
	}
	if _, err := parseSource("petkit://main?talkback=maybe"); err == nil {
		t.Fatalf("bad talkback should error")
	}
}

func TestParseSourceSnapshot(t *testing.T) {
	// Native get_jpeg snapshots are OFF by default (they disrupt the live
	// encoder on at least the localkit D4SH2 firmware).
	if cfg, err := parseSource("petkit://main"); err != nil || cfg.snapshot {
		t.Fatalf("default snapshot: got %v, %v", cfg.snapshot, err)
	}
	// ?snapshot=1 opts in.
	if cfg, err := parseSource("petkit://main?snapshot=1"); err != nil || !cfg.snapshot {
		t.Fatalf("snapshot=1: got %v, %v", cfg.snapshot, err)
	}
	if _, err := parseSource("petkit://main?snapshot=perhaps"); err == nil {
		t.Fatalf("bad snapshot should error")
	}
}

func TestParseSourceForceIDR(t *testing.T) {
	// Force-keyframe on connect is on by default.
	if cfg, err := parseSource("petkit://main"); err != nil || !cfg.forceIDR {
		t.Fatalf("default idr: got %v, %v", cfg.forceIDR, err)
	}
	if cfg, err := parseSource("petkit://main?idr=0"); err != nil || cfg.forceIDR {
		t.Fatalf("idr=0: got %v, %v", cfg.forceIDR, err)
	}
	if _, err := parseSource("petkit://main?idr=nah"); err == nil {
		t.Fatalf("bad idr should error")
	}
}

func TestMsgRequestIDROverride(t *testing.T) {
	if got := msgRequestIDR(); got != msgRequestIDRDefault {
		t.Fatalf("default idr msg id: got 0x%x want 0x%x", got, msgRequestIDRDefault)
	}
	if msgRequestIDRDefault != 1 {
		t.Fatalf("request_IDR default should be verified value 1, got %d", msgRequestIDRDefault)
	}
	t.Setenv("PETKIT_IDR_MSGID", "0x2a")
	if got := msgRequestIDR(); got != 0x2a {
		t.Fatalf("hex override: got 0x%x", got)
	}
	t.Setenv("PETKIT_IDR_MSGID", "junk")
	if got := msgRequestIDR(); got != msgRequestIDRDefault {
		t.Fatalf("bad override fallback: got 0x%x", got)
	}
}

func TestParseSourceDebug(t *testing.T) {
	// Off unless asked (env default is false in the test process).
	if cfg, err := parseSource("petkit://main"); err != nil || cfg.debug {
		t.Fatalf("default debug: got %v, %v", cfg.debug, err)
	}
	if cfg, err := parseSource("petkit://main?debug=1"); err != nil || !cfg.debug {
		t.Fatalf("debug=1: got %v, %v", cfg.debug, err)
	}
	if _, err := parseSource("petkit://main?debug=yesno"); err == nil {
		t.Fatalf("bad debug should error")
	}
}

func TestMsgGetJpegPlane(t *testing.T) {
	// Verified msg ids: main=4 -> snap_main.jpeg, sub=6 -> snap_sub.jpeg.
	if got := msgGetJpeg("main"); got != msgGetJpegMain {
		t.Fatalf("main msg id: got %d want %d", got, msgGetJpegMain)
	}
	if got := msgGetJpeg("sub"); got != msgGetJpegSub {
		t.Fatalf("sub msg id: got %d want %d", got, msgGetJpegSub)
	}
	// Env override forces a specific id regardless of plane.
	t.Setenv("PETKIT_JPEG_MSGID", "0x1f")
	if got := msgGetJpeg("main"); got != 0x1f {
		t.Fatalf("hex override: got 0x%x", got)
	}
	// A bad value falls back to the per-plane default rather than erroring.
	t.Setenv("PETKIT_JPEG_MSGID", "notanumber")
	if got := msgGetJpeg("sub"); got != msgGetJpegSub {
		t.Fatalf("bad override fallback: got %d", got)
	}
}

func TestParseSourceLayoutOverrides(t *testing.T) {
	// A single field tweak on top of a named profile (hex).
	cfg, err := parseSource("petkit://main?layout=t7&flags_off=0x1c")
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if cfg.layout.offFlags != 0x1c || cfg.layout.hdrSize != 0x35 {
		t.Fatalf("override not applied: %+v", cfg.layout)
	}
	if cfg.layout.name != "t7+custom" {
		t.Fatalf("custom name: got %q", cfg.layout.name)
	}

	// A fully custom descriptor (decimal + hex mixed) on the ARM default.
	cfg, err = parseSource("petkit://sub?hdr_size=0x40&size_off=4&flags_off=0x24&type_off=0x22")
	if err != nil {
		t.Fatalf("custom: %v", err)
	}
	if cfg.layout.hdrSize != 0x40 || cfg.layout.offSize != 4 ||
		cfg.layout.offFlags != 0x24 || cfg.layout.offType != 0x22 {
		t.Fatalf("custom layout wrong: %+v", cfg.layout)
	}

	// Overrides that push a field past the descriptor must be rejected, not
	// silently corrupt the ring walk.
	if _, err := parseSource("petkit://main?layout=t7&flags_off=0x34"); err == nil {
		t.Fatalf("out-of-range flags_off should error (0x34+2 > 0x35)")
	}
	if _, err := parseSource("petkit://main?hdr_size=0"); err == nil {
		t.Fatalf("zero hdr_size should error")
	}
	if _, err := parseSource("petkit://main?flags_off=notanumber"); err == nil {
		t.Fatalf("non-numeric offset should error")
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
