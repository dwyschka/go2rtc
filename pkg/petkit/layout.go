package petkit

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// frameLayout captures the per-firmware frame-descriptor field offsets. It is
// the ONE thing that differs between the camera SoC families we support: the
// shared-memory control block and the reader-slot array are byte-for-byte
// identical across firmwares (verified against libbase.so on both the ARM/AXERA
// and the Ingenic-T7/MIPS builds — same 0x3E8 control block, same 0x2C reader
// slots at +0x2C, same head/tail/num offsets). Only the per-frame descriptor
// that precedes each payload in the ring is laid out differently.
//
// Offsets that are absent on a given firmware are set to -1 and skipped by the
// writer.
type frameLayout struct {
	name    string // profile name, for diagnostics
	hdrSize int    // descriptor size in bytes (mbuffer copies exactly this many)

	// speaker reports whether this device has a loudspeaker, i.e. whether the
	// talkback (browser mic -> camera speaker) backchannel is meaningful. It is
	// the default for config.talkback and is overridable per stream with
	// ?talkback=0|1. The Ingenic-T7 has a mic but no speaker, so it defaults off.
	speaker bool

	// Read-side fields (present on every firmware).
	offNum   int // uint32 sequence number
	offSize  int // uint32 payload length
	offIndex int // uint32 producer frame/slice counter
	offPTS   int // uint64 presentation time (microseconds)
	offType  int // byte frame_type: 1 = video I-frame, 2 = video P-frame
	offFlags int // uint16 type_flags: the media-type bitmask the reader filters on
	offSPS   int // uint16 H.264 SPS length (keyframes); -1 if absent
	offPPS   int // uint16 H.264 PPS length (keyframes); -1 if absent

	// Write-side (talkback) descriptor extras. The camera's ring writer only
	// needs num/size/type/flags, but the ARM firmware's media daemon also reads
	// a codec tag + sample-count to configure its AAC decoder. -1 = the field
	// does not exist in this firmware's descriptor and must not be written.
	offWallSec   int // uint32 wall-clock seconds
	offCaptureUS int // uint64 local capture timestamp (microseconds)
	offCodec     int // byte codec tag (4 = AAC)
	offSamples   int // uint16 samples per frame (0x400 for AAC-LC)
	offKHz       int // uint16 sample rate marker (0x10)
}

// layoutARM is the descriptor used by the ARM/AXERA ("D4SH") firmware. It is the
// historical default: every offset here matches agora's __on_audio_data writer
// and tserver's reader on that SoC. A 64-bit capture timestamp sits at +0x18,
// pushing the type/flags fields to +0x20/+0x22.
//
// The "d4sh2" firmware variant shares this descriptor byte-for-byte — verified
// against tserver_d4sh2's mbuffer_read_frame (FUN_00014c00): it copies a 0x38
// header, filters on the u16 type_flags at +0x22, reads num@0x00 / size@0x04,
// and masks the ring offset with 0x7FFFFF (an 8 MiB ring) — all identical to
// AXERA. So "d4sh2" is just an alias here, not a separate profile.
var layoutARM = frameLayout{
	name:    "arm",
	hdrSize: 0x38,
	speaker: true,
	offNum:  0x00, offSize: 0x04, offIndex: 0x08, offPTS: 0x10,
	offType: 0x20, offFlags: 0x22, offSPS: 0x32, offPPS: 0x34,
	offWallSec: 0x0c, offCaptureUS: 0x18, offCodec: 0x21, offSamples: 0x2e, offKHz: 0x30,
}

// layoutT7 is the descriptor used by the Ingenic T7 (little-endian MIPS)
// firmware. Verified byte-for-byte from t7_libbase.so:
//
//   - mbuffer_read_frame / mbuffer_write_frame copy 0x35 (not 0x38) bytes per
//     descriptor and the total frame size is payload + 0x35.
//   - The reader's filter compares a uint16 media-type bitmask at descriptor
//     +0x1A against the slot's mask (reader+0x12).
//   - The keyframe check reads the frame_type byte at +0x18 (== 1 for I-frames).
//   - avget_get_sample in t7_tserver derives SPS/PPS lengths from +0x2F/+0x31.
//
// There is no 64-bit capture timestamp before the type/flags block, so every
// field from +0x18 onward sits 8 bytes earlier than on ARM. The extra AAC
// decoder-config fields the ARM media daemon reads are not part of this smaller
// descriptor, so the talkback writer omits them.
var layoutT7 = frameLayout{
	name:    "t7",
	hdrSize: 0x35,
	speaker: false, // mic only, no loudspeaker -> no talkback backchannel
	offNum:  0x00, offSize: 0x04, offIndex: 0x08, offPTS: 0x10,
	offType: 0x18, offFlags: 0x1a, offSPS: 0x2f, offPPS: 0x31,
	offWallSec: -1, offCaptureUS: -1, offCodec: -1, offSamples: -1, offKHz: -1,
}

// layoutW7H is the descriptor used by the "w7h" firmware (ARM v8, verified
// byte-for-byte from tserver_w7h, statically linked libbase). It is an
// ARM-family device with a loudspeaker (talkback works), but its frame
// descriptor is 5 bytes larger than the AXERA one and its ring is 2 MiB (like
// the Ingenic-T7), not 8 MiB — the ring size is derived from fstat at runtime,
// so only the descriptor offsets need pinning here.
//
// Verified against tserver_w7h:
//
//   - mbuffer_read_frame (FUN_0001bd00) copies 0x3D bytes per descriptor and
//     advances the ring by 0x3D; the ring offset is masked with 0x1FFFFF, i.e. a
//     2 MiB ring (mmap size 0x2003E8 = 0x3E8 control block + 0x200000).
//   - The reader's media-type filter compares the uint16 at descriptor +0x22
//     against the slot mask (reader+0x12) — identical to AXERA.
//   - The frame consumer (FUN_00012214) reads num@0x00, size@0x04, index@0x08,
//     pts(u64)@0x10, frame_type byte@0x20 (1=I, 2=P), type_flags u16@0x22,
//     SPS len u16@0x37, PPS len u16@0x39.
//
// Everything through the type_flags field at +0x22 is byte-identical to
// layoutARM; the whole tail (AAC sample hints, SPS/PPS, total size) sits exactly
// 5 bytes later. The write-side descriptor is pinned from agora_w7h's
// __on_audio_data (FUN_00014a04), the ground-truth ring writer: it fills
// num@0, size@4, seq@8, wall_sec@0x0c, pts_us@0x10, capture_us@0x18, a packed
// type(0)/codec(4)/type_flags(0x0002) word at 0x20, then samples=0x0400 as a
// u16 at 0x33 and the sample-rate marker 0x10 as a u16 at 0x35 (both +5 from
// ARM's 0x2e/0x30). The w7h media daemon's "auido-out" reader
// (media_w7h FUN_0002d0d0) actually ignores the sample hints and streams the
// payload straight into an AX_ADEC channel opened with enTransType=2
// (TT_MP4_ADTS) — so talkback needs 16 kHz mono ADTS-AAC, which is our default.
var layoutW7H = frameLayout{
	name:    "w7h",
	hdrSize: 0x3d,
	speaker: true,
	offNum:  0x00, offSize: 0x04, offIndex: 0x08, offPTS: 0x10,
	offType: 0x20, offFlags: 0x22, offSPS: 0x37, offPPS: 0x39,
	offWallSec: 0x0c, offCaptureUS: 0x18, offCodec: 0x21, offSamples: 0x33, offKHz: 0x35,
}

// selectLayout maps a profile name (case-insensitive) to its frame layout.
// Aliases group firmwares by the SoC family whose descriptor they share.
func selectLayout(name string) (frameLayout, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "arm", "axera", "d4sh", "d4sh2":
		return layoutARM, nil
	case "t7", "mips", "mipsle", "ingenic", "d4":
		return layoutT7, nil
	case "w7h", "w7", "w7hc":
		return layoutW7H, nil
	default:
		return frameLayout{}, fmt.Errorf(
			"petkit: unknown frame layout %q (want arm|axera, t7|mips or w7h)", name)
	}
}

// resolveLayout picks the descriptor layout. An explicit name (from the
// ?layout= URL query) wins; otherwise the PETKIT_LAYOUT environment variable;
// otherwise the ARM default. This lets a single build serve either SoC family.
func resolveLayout(explicit string) (frameLayout, error) {
	if strings.TrimSpace(explicit) == "" {
		explicit = os.Getenv("PETKIT_LAYOUT")
	}
	return selectLayout(explicit)
}

// layoutFromQuery resolves the base layout profile (?layout= / PETKIT_LAYOUT)
// then applies any per-field offset overrides taken from the stream URL query.
// This means a device whose descriptor matches neither built-in profile can be
// described entirely in go2rtc.yaml — no code change, no new build. Every offset
// accepts decimal or 0x-hex; -1 marks a field as absent.
//
//	petkit://main?layout=t7                          # named profile
//	petkit://main?layout=t7&flags_off=0x1c           # profile + one tweak
//	petkit://main?hdr_size=0x40&size_off=4&flags_off=0x24&type_off=0x22
func layoutFromQuery(q url.Values) (frameLayout, error) {
	l, err := resolveLayout(q.Get("layout"))
	if err != nil {
		return frameLayout{}, err
	}

	overrides := []struct {
		key string
		fld *int
	}{
		{"hdr_size", &l.hdrSize},
		{"num_off", &l.offNum},
		{"size_off", &l.offSize},
		{"index_off", &l.offIndex},
		{"pts_off", &l.offPTS},
		{"type_off", &l.offType},
		{"flags_off", &l.offFlags},
		{"sps_off", &l.offSPS},
		{"pps_off", &l.offPPS},
	}

	custom := false
	for _, o := range overrides {
		v := q.Get(o.key)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 0, 32)
		if err != nil {
			return frameLayout{}, fmt.Errorf("petkit: bad %s=%q: %w", o.key, v, err)
		}
		*o.fld = int(n)
		custom = true
	}

	if custom {
		if err := l.validate(); err != nil {
			return frameLayout{}, err
		}
		l.name += "+custom"
	}
	return l, nil
}

// validate guards a hand-tuned layout: the descriptor size must be sane and no
// present read field may reach past it (an off-by-one there silently corrupts
// the ring walk). Fields set to -1 (absent) are skipped.
func (l frameLayout) validate() error {
	if l.hdrSize <= 0 || l.hdrSize > 0x200 {
		return fmt.Errorf("petkit: hdr_size 0x%x out of range (1..0x200)", l.hdrSize)
	}
	fields := []struct {
		name  string
		off   int
		width int
	}{
		{"num_off", l.offNum, 4}, {"size_off", l.offSize, 4},
		{"index_off", l.offIndex, 4}, {"pts_off", l.offPTS, 8},
		{"type_off", l.offType, 1}, {"flags_off", l.offFlags, 2},
		{"sps_off", l.offSPS, 2}, {"pps_off", l.offPPS, 2},
	}
	for _, f := range fields {
		if f.off < 0 {
			continue // field marked absent
		}
		if f.off+f.width > l.hdrSize {
			return fmt.Errorf("petkit: %s=0x%x (+%d) exceeds hdr_size 0x%x",
				f.name, f.off, f.width, l.hdrSize)
		}
	}
	return nil
}

// parseFrame decodes the descriptor at the front of a ring slot using this
// firmware's field offsets. Absent optional fields (offset < 0) decode as 0.
func (l frameLayout) parseFrame(h []byte) Frame {
	return Frame{
		Num:   leU32(h, l.offNum),
		Index: leU32(h, l.offIndex),
		PTS:   leU64(h, l.offPTS),
		Type:  leByte(h, l.offType),
		Flags: leU16(h, l.offFlags),
		SPS:   leU16(h, l.offSPS),
		PPS:   leU16(h, l.offPPS),
	}
}
