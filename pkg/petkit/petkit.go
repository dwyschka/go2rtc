// Package petkit is an on-device replacement for the Petkit camera "tserver"
// binary. Instead of connecting to tserver over HTTP, this driver reads the
// H.264/AAC media frames directly out of the shared-memory ring buffer that the
// camera's media pipeline publishes, exactly like tserver does — then exposes
// them to go2rtc's normal producer/consumer graph (RTSP, WebRTC, HLS, …).
//
// It is meant to run ON the Petkit device (little-endian MIPS "D4" or ARM
// "D4SH" firmware), so the media plumbing is Linux-only. On other platforms
// Dial returns an error.
//
// Reverse-engineered from the MIPS/ARM tserver binaries and libbase.so:
//
//   - Ring buffer:  POSIX shm object "/media_buffer_frame_buf".
//     Layout: a 0x3E8 (1000-byte) control block followed by a power-of-two
//     ring (2 MiB on Ingenic-T7/MIPS, 8 MiB on ARM/AXERA). Size is discovered
//     via fstat, so the same code works on both builds. The control block and
//     reader-slot layout are byte-for-byte identical across firmwares.
//   - Reader:       a consumer registers a 0x2C-byte slot (name "ts-server")
//     in the control block and receives only frames whose type bits match its
//     filter mask.
//   - Dispatch:     on start, a message is sent to POSIX mqueue
//     "/msg_dispatch_1" telling the camera pipeline which plane/audio to emit.
//   - Frames:       video is H.264 Annex-B, audio is AAC in ADTS. The per-frame
//     descriptor differs by SoC family (see frameLayout / layout.go): the ARM
//     build uses a 0x38-byte descriptor with type/flags at +0x20/+0x22, the
//     Ingenic-T7 build a 0x35-byte descriptor with them at +0x18/+0x1A. Select
//     with ?layout=arm|t7 or the PETKIT_LAYOUT env var (default arm).
//
// URL format:
//
//	petkit://main          main (high-quality) video + audio
//	petkit://main?audio=0  main video only
//	petkit://sub           sub (low-quality) video + audio
//	petkit://sub?audio=0   sub video only
//
// The host component is ignored (the buffer is always local shared memory);
// "petkit://main" and "petkit:main" are equivalent. Default plane is main and
// audio defaults to on.
package petkit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// media_type bit flags (frame header type_flags and reader filter mask).
const (
	mediaAudio = 0x1 // bit 0: audio frame
	mediaMain  = 0x4 // bit 2: main-stream video
	mediaSub   = 0x8 // bit 3: sub-stream video
)

// config describes a decoded petkit:// source.
type config struct {
	plane     string // "main" or "sub"
	audio     bool
	talkback  bool        // advertise the browser-mic -> speaker backchannel
	mediaType uint32      // filter mask + dispatch payload: 4/5/8/9
	layout    frameLayout // per-firmware frame-descriptor layout
	debug     bool        // verbose driver tracing for this source
	snapshot  bool        // advertise a native JPEG track (device HW encoder)
	forceIDR  bool        // request a hardware keyframe on connect + on resync
}

// parseSource decodes a petkit:// URL into the plane + audio selection and the
// resulting media_type bitmask. Portable (no device access).
func parseSource(source string) (config, error) {
	// Accept both "petkit://main" and "petkit:main".
	raw := source
	if !strings.Contains(raw, "://") {
		raw = strings.Replace(raw, "petkit:", "petkit://", 1)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return config{}, err
	}

	// The plane can be given either as the host ("petkit://main") or as the
	// first path segment ("petkit://localhost/main").
	plane := u.Host
	if plane == "" || plane == "localhost" || plane == "127.0.0.1" {
		plane = strings.Trim(u.Path, "/")
	}
	// Strip a container-style extension if the user copied a tserver path
	// (main.flv / main.ts / sub.ts) — the container is irrelevant here.
	if i := strings.IndexByte(plane, '.'); i > 0 {
		plane = plane[:i]
	}

	var videoBit uint32
	switch plane {
	case "", "main":
		plane = "main"
		videoBit = mediaMain
	case "sub":
		videoBit = mediaSub
	default:
		return config{}, errors.New("petkit: unknown stream plane: " + plane)
	}

	// Audio is on by default; ?audio=0 disables it.
	audio := u.Query().Get("audio") != "0"

	mediaType := videoBit
	if audio {
		mediaType |= mediaAudio
	}

	// The frame-descriptor layout differs by camera SoC family. Select it per
	// source via ?layout=arm|t7 (falling back to the PETKIT_LAYOUT env var and
	// then the ARM default), with optional per-field offset overrides in the
	// query for devices that match neither built-in profile.
	layout, err := layoutFromQuery(u.Query())
	if err != nil {
		return config{}, err
	}

	// Talkback (browser mic -> camera speaker) defaults to whether the device
	// has a speaker at all; ?talkback=0|1 overrides. A mic-only device (T7) thus
	// advertises no dead backchannel.
	talkback := layout.speaker
	if v := u.Query().Get("talkback"); v != "" {
		if talkback, err = strconv.ParseBool(v); err != nil {
			return config{}, fmt.Errorf("petkit: bad talkback=%q: %w", v, err)
		}
	}

	// Verbose driver tracing: ?debug=1 per stream, else the PETKIT_DEBUG env var.
	debug := envDebug
	if v := u.Query().Get("debug"); v != "" {
		if debug, err = strconv.ParseBool(v); err != nil {
			return config{}, fmt.Errorf("petkit: bad debug=%q: %w", v, err)
		}
	}

	// Native JPEG snapshots via the camera's hardware get_jpeg dispatch. OFF by
	// default: on at least the localkit D4SH2 firmware, get_jpeg
	// (media_venc_get_jpeg_snap) reconfigures the LIVE IVPS group
	// (AX_IVPS_SetPipelineAttr) to grab a frame, which starves the running
	// encoders and trips media's watchdog reboot. The safe replacement is a
	// pure-Go H.264 keyframe decoder (decode the IDR we already read from the
	// ring). Opt in with ?snapshot=1 only on firmware known to tolerate it.
	snapshot := false
	if v := u.Query().Get("snapshot"); v != "" {
		if snapshot, err = strconv.ParseBool(v); err != nil {
			return config{}, fmt.Errorf("petkit: bad snapshot=%q: %w", v, err)
		}
	}

	// Force a hardware keyframe on connect (and on resync). On by default;
	// ?idr=0 disables it for firmwares that lack the request_IDR dispatch.
	forceIDR := true
	if v := u.Query().Get("idr"); v != "" {
		if forceIDR, err = strconv.ParseBool(v); err != nil {
			return config{}, fmt.Errorf("petkit: bad idr=%q: %w", v, err)
		}
	}

	return config{
		plane: plane, audio: audio, talkback: talkback,
		mediaType: mediaType, layout: layout, debug: debug,
		snapshot: snapshot, forceIDR: forceIDR,
	}, nil
}

// Frame is one media unit copied out of the ring buffer.
type Frame struct {
	Num   uint32 // sequence number
	Index uint32 // producer frame/slice counter
	PTS   uint64 // presentation time, microseconds
	Type  uint8  // 1 = video I-frame, 2 = video P-frame, 0 = audio/other
	Flags uint16 // type_flags: bit0 audio, bit2 main video, bit3 sub video
	SPS   uint16 // H.264 SPS length prefixed to Data (keyframes)
	PPS   uint16 // H.264 PPS length prefixed to Data (keyframes)
	Data  []byte // payload (Annex-B H.264 or ADTS AAC)
}

// Little-endian field readers that treat a negative offset (a field absent in
// this firmware's descriptor) or an out-of-range offset as zero, so a shorter
// descriptor layout can never index past the copied header bytes.

func leByte(h []byte, off int) uint8 {
	if off < 0 || off >= len(h) {
		return 0
	}
	return h[off]
}

func leU16(h []byte, off int) uint16 {
	if off < 0 || off+2 > len(h) {
		return 0
	}
	return binary.LittleEndian.Uint16(h[off:])
}

func leU32(h []byte, off int) uint32 {
	if off < 0 || off+4 > len(h) {
		return 0
	}
	return binary.LittleEndian.Uint32(h[off:])
}

func leU64(h []byte, off int) uint64 {
	if off < 0 || off+8 > len(h) {
		return 0
	}
	return binary.LittleEndian.Uint64(h[off:])
}

// Matching writers. A negative offset (field absent in this firmware's
// descriptor) or an out-of-range offset is a no-op, so the write path never
// touches bytes outside the descriptor it allocated.

func putByte(h []byte, off int, v uint8) {
	if off < 0 || off >= len(h) {
		return
	}
	h[off] = v
}

func putU16(h []byte, off int, v uint16) {
	if off < 0 || off+2 > len(h) {
		return
	}
	binary.LittleEndian.PutUint16(h[off:], v)
}

func putU32(h []byte, off int, v uint32) {
	if off < 0 || off+4 > len(h) {
		return
	}
	binary.LittleEndian.PutUint32(h[off:], v)
}

func putU64(h []byte, off int, v uint64) {
	if off < 0 || off+8 > len(h) {
		return
	}
	binary.LittleEndian.PutUint64(h[off:], v)
}

// ringRead copies n bytes out of a power-of-two ring starting at byte offset
// off, wrapping at the ring boundary.
func ringRead(ring []byte, off, n uint32) []byte {
	size := uint32(len(ring))
	out := make([]byte, n)
	if off+n <= size {
		copy(out, ring[off:off+n])
	} else {
		first := size - off
		copy(out, ring[off:size])
		copy(out[first:], ring[:n-first])
	}
	return out
}

// cstr returns the NUL-terminated string at the start of b.
func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
