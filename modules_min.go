//go:build petkit_min

// Minimal on-device build for the Petkit driver. Only the petkit source plus
// the streaming outputs needed to view/forward it are registered — every
// third-party camera integration, cloud connector, exec/ffmpeg and hardware
// source is dropped to shrink the binary for the device's limited flash.
//
//	go build -tags petkit_min -ldflags="-s -w" -trimpath .
package main

import (
	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/hls"
	"github.com/AlexxIT/go2rtc/internal/mjpeg"
	"github.com/AlexxIT/go2rtc/internal/mp4"
	"github.com/AlexxIT/go2rtc/internal/petkit"
	"github.com/AlexxIT/go2rtc/internal/rtsp"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/internal/webrtc"
)

// activeModules returns the minimal Petkit-device subsystem set.
func activeModules() []module {
	return []module{
		{"", app.Init},    // config and logs
		{"api", api.Init}, // HTTP API server
		{"ws", ws.Init},   // WebSocket API
		{"", streams.Init},
		// Source
		{"petkit", petkit.Init}, // shared-memory Petkit camera source
		// Outputs
		{"rtsp", rtsp.Init},     // RTSP server
		{"webrtc", webrtc.Init}, // WebRTC server (browser live view)
		{"mp4", mp4.Init},       // MP4 API
		{"hls", hls.Init},       // HLS API
		{"mjpeg", mjpeg.Init},   // MJPEG / snapshot API
	}
}
