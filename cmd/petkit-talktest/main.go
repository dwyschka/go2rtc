//go:build linux

// Command petkit-talktest plays a test tone on the Petkit camera speaker via
// the talkback path, to validate the on-device audio-out without a browser.
//
//	./petkit-talktest [seconds] [freqHz]
//
// Run as root on the device (needs the shared-memory ring + mqueue).
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/petkit"
)

func main() {
	secs := 3.0
	freq := 1000.0
	if len(os.Args) > 1 {
		if v, err := strconv.ParseFloat(os.Args[1], 64); err == nil {
			secs = v
		}
	}
	if len(os.Args) > 2 {
		if v, err := strconv.ParseFloat(os.Args[2], 64); err == nil {
			freq = v
		}
	}

	fmt.Printf("playing %.0f Hz tone for %.1fs on the camera speaker...\n", freq, secs)
	if err := petkit.SelfTestTone(time.Duration(secs*float64(time.Second)), freq); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Println("done")
}
