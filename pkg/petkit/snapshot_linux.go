package petkit

import (
	"errors"
	"os"
	"time"

	"github.com/pion/rtp"
)

// Native JPEG snapshots.
//
// The camera's `media` daemon (DISPATCH_RECEIVER_MEDIA) exposes a hardware JPEG
// path, reverse-engineered from media_arm / the Ingenic `media` binary:
//
//	dispatch_handler_get_jpeg
//	  -> media_venc_get_jpeg_snap
//	     -> AX_VENC_JpegEncodeOneFrame   (AXERA)  / IMP JPEG encoder (Ingenic)
//	     -> writes /tmp/snap_main.jpeg or /tmp/snap_sub.jpeg
//
// So go2rtc never needs to decode H.264 or shell out to ffmpeg for a snapshot:
// it asks the daemon to encode one frame with its hardware JPEG unit and reads
// the resulting file. pktool's `snap` command is the reference caller.
//
// The exact dispatch msg_id for get_jpeg (msgGetJpegDefault) is the one value
// that must be confirmed against the firmware — see snapshot.go, which holds the
// device-independent constants and env overrides so they stay unit-testable on
// any host. Everything below is Linux-only (dispatch + file IO).

// snapshotJPEG triggers the camera's hardware JPEG encoder and returns the
// freshly written JPEG bytes. It records the file's mtime before triggering and
// waits for it to advance, so a stale file from a previous snapshot is never
// mistaken for the new one. If the daemon never rewrites the file it falls back
// to whatever is on disk (better a slightly stale frame than none).
func (p *Producer) snapshotJPEG() ([]byte, error) {
	path := snapshotPathFor(p.cfg.plane)

	var before time.Time
	if fi, err := os.Stat(path); err == nil {
		before = fi.ModTime()
	}

	// Ask module 1 to encode one frame. The plane is selected by the msg_id
	// itself (4=main, 6=sub) and the handler MUST get an empty payload — with a
	// payload it would read the output path from payload+0x2e instead.
	if err := dispatchSendFrom(dispatchJpegModule, msgGetJpeg(p.cfg.plane), dispatchSrcModule, nil); err != nil {
		p.cfg.dbg("snapshot dispatch failed (best-effort): %v", err)
	}

	deadline := time.Now().Add(jpegTimeout)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 && fi.ModTime().After(before) {
			b, err := os.ReadFile(path)
			if err == nil && len(b) > 0 {
				p.cfg.dbg("snapshot: %d bytes from %s", len(b), path)
				return b, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Fall back to the existing file if the trigger produced nothing new.
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		p.cfg.dbg("snapshot: trigger produced nothing new, serving stale %s (%d bytes)", path, len(b))
		return b, nil
	}
	return nil, errors.New("petkit: snapshot timed out — no JPEG written " +
		"(get_jpeg msg_id may differ on this firmware; set PETKIT_JPEG_MSGID)")
}

// pumpJPEG feeds the JPEG receiver on demand. It writes one frame immediately
// (so a frame.jpeg snapshot returns fast) and then refreshes on a slow cadence
// for any live stream.mjpeg consumer, until the producer stops.
func (p *Producer) pumpJPEG() {
	for {
		if b, err := p.snapshotJPEG(); err == nil && len(b) > 0 {
			p.jpeg.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Timestamp: nowRTP(p.jpeg.Codec.ClockRate)},
				Payload: b,
			})
		}
		select {
		case <-p.jpegStop:
			return
		case <-time.After(jpegRefresh):
		}
	}
}
