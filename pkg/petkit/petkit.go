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
//     ring (4 MiB on MIPS, 8 MiB on ARM). Size is discovered via fstat, so the
//     same code works on both builds.
//   - Reader:       a consumer registers a 0x2C-byte slot (name "ts-server")
//     in the control block and receives only frames whose type bits match its
//     filter mask.
//   - Dispatch:     on start, a message is sent to POSIX mqueue
//     "/msg_dispatch_1" telling the camera pipeline which plane/audio to emit.
//   - Frames:       video is H.264 Annex-B, audio is AAC in ADTS. Presentation
//     timestamps are 64-bit microseconds in the frame header.
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
	"net/url"
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
	mediaType uint32 // filter mask + dispatch payload: 4/5/8/9
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

	return config{plane: plane, audio: audio, mediaType: mediaType}, nil
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

// parseFrameHeader decodes the 0x38-byte frame header at the front of a ring
// slot. Field offsets are fixed across the MIPS and ARM builds.
func parseFrameHeader(h []byte) Frame {
	return Frame{
		Num:   binary.LittleEndian.Uint32(h[0x00:]),
		Index: binary.LittleEndian.Uint32(h[0x08:]),
		PTS:   binary.LittleEndian.Uint64(h[0x10:]),
		Type:  h[0x20],
		Flags: binary.LittleEndian.Uint16(h[0x22:]),
		SPS:   binary.LittleEndian.Uint16(h[0x32:]),
		PPS:   binary.LittleEndian.Uint16(h[0x34:]),
	}
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
