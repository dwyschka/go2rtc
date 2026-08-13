package petkit

import (
	"os"
	"strconv"
	"time"
)

// Device-independent snapshot constants + env overrides. Kept build-tag-free so
// they are unit-testable on any host; the Linux-only dispatch/file/pump logic
// lives in snapshot_linux.go.

// get_jpeg dispatch ids, verified against media_arm + media_w7h in Ghidra:
// dispatch_handler_get_jpeg branches on the msg_id itself with NO payload —
// msg 4 encodes the main plane to /tmp/snap_main.jpeg, msg 6 the sub plane to
// /tmp/snap_sub.jpeg. (With a payload it would instead read the output path from
// payload+0x2e, which we never want.)
const (
	msgGetJpegMain uint16 = 4 // -> /tmp/snap_main.jpeg (HD)
	msgGetJpegSub  uint16 = 6 // -> /tmp/snap_sub.jpeg (SD)

	// dispatchJpegModule is the destination module that owns get_jpeg. The media
	// daemon registers as module 1 (same queue as speak_start / request_IDR).
	dispatchJpegModule uint16 = 1
)

// jpegTimeout bounds how long snapshotJPEG waits for the daemon to (re)write the
// snapshot file after the trigger. The camera's get_jpeg handler
// (media_venc_get_jpeg_snap) enables an IVPS channel on demand and grabs one
// frame with its OWN 2s AX_IVPS_GetChnFrame timeout, then JPEG-encodes and
// writes the file — so the whole device-side operation can take just over 2s.
// We must wait comfortably longer than that or we race it and serve the stale
// (often blank) previous file.
const jpegTimeout = 5 * time.Second

// jpegRefresh is the cadence at which a live JPEG consumer (stream.mjpeg) gets a
// fresh frame. Each snapshot toggles an IVPS channel on/off on the camera, so a
// fast cadence thrashes the pipeline; a slow preview refresh is plenty and keeps
// the snapshot path from fighting the live stream. A single frame.jpeg snapshot
// only ever reads the first one.
const jpegRefresh = 5 * time.Second

// msgGetJpeg returns the get_jpeg dispatch id for a plane ("main"/"sub"),
// honouring the PETKIT_JPEG_MSGID override (decimal or 0x-hex) for firmwares that
// use a different id. A malformed override falls back to the per-plane default.
func msgGetJpeg(plane string) uint16 {
	if s := os.Getenv("PETKIT_JPEG_MSGID"); s != "" {
		if v, err := strconv.ParseUint(s, 0, 16); err == nil {
			return uint16(v)
		}
	}
	if plane == "sub" {
		return msgGetJpegSub
	}
	return msgGetJpegMain
}

// snapshotPathFor is the file the daemon writes the encoded JPEG to for a given
// plane. PETKIT_JPEG_PATH overrides it wholesale for odd firmwares.
func snapshotPathFor(plane string) string {
	if s := os.Getenv("PETKIT_JPEG_PATH"); s != "" {
		return s
	}
	return "/tmp/snap_" + plane + ".jpeg"
}
