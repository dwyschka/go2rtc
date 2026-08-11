package petkit

import (
	"os"
	"strconv"
)

// Force-keyframe (IDR) request — verified against media_arm + media_w7h in Ghidra.
//
// The media daemon registers dispatch_handler_request_IDR on **msg_id 1**,
// module 1. The handler REQUIRES a payload: its first u16 is a channel bitmask —
// bit 2 (0x04) → AX_VENC_RequestIDR(0) (main/chn0), bit 3 (0x08) → chn1 (sub),
// bit 4 (0x10) → chn2. Those are exactly the media_type plane bits, so we send
// cfg.mediaType as the payload and the encoder re-emits an SPS+IDR on the plane
// we stream. A NULL/empty payload is rejected by the handler ("data is NULL").
//
// Forcing an IDR on connect means the probe — and any joining WebRTC/MSE client —
// gets a keyframe immediately instead of waiting out the encoder's natural GOP,
// the usual cause of a stream that "hangs" for a few seconds on connect. We also
// fire one when the ring laps so resync after frame loss is fast.
//
// (This is the same message the driver already sent at Dial as the misnamed
// "set frame type" — it was request_IDR all along.)

const (
	// msgRequestIDRDefault is the verified request_IDR dispatch id (media_arm +
	// media_w7h). Override with PETKIT_IDR_MSGID for other firmwares.
	msgRequestIDRDefault uint16 = 1

	// dispatchIDRModule is the destination module that owns request_IDR — the
	// media daemon (module 1), same queue as get_jpeg / speak_start.
	dispatchIDRModule uint16 = 1
)

// msgRequestIDR returns the request_IDR dispatch id, honouring the
// PETKIT_IDR_MSGID override (decimal or 0x-hex). A malformed value falls back to
// the default.
func msgRequestIDR() uint16 {
	if s := os.Getenv("PETKIT_IDR_MSGID"); s != "" {
		if v, err := strconv.ParseUint(s, 0, 16); err == nil {
			return uint16(v)
		}
	}
	return msgRequestIDRDefault
}
