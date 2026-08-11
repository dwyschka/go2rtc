package petkit

import (
	"log"
	"os"
	"strconv"
)

// envDebug is the process-wide default for driver tracing, set from
// PETKIT_DEBUG=1. A per-stream ?debug=1 (config.debug) overrides it for a single
// source. Output goes to stderr, which go2rtc captures into go2rtc.log, so it
// shows up alongside the normal log without needing a zerolog module wired into
// the portable pkg layer.
var envDebug = func() bool {
	v, _ := strconv.ParseBool(os.Getenv("PETKIT_DEBUG"))
	return v
}()

// dbg prints a driver trace line when tracing is enabled for this source. It is
// a no-op (and does not format its args) otherwise, so it is cheap to leave in
// hot paths.
func (c config) dbg(format string, args ...any) {
	if c.debug {
		log.Printf("[petkit] "+format, args...)
	}
}
