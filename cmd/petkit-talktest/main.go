//go:build linux

// Command petkit-talktest validates the Petkit camera talkback audio-out
// without a browser: it runs diagnostics then plays either a test tone or an
// existing ADTS-AAC file through the shared-memory ring.
//
//	./petkit-talktest [seconds] [freqHz]     # play a sine tone
//	./petkit-talktest /audio/audio_test.aac  # play an ADTS-AAC file
//
// Run as root on the device (needs the shared-memory ring + mqueue).
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/petkit"
)

func main() {
	fmt.Println("=== talkback diagnostics ===")
	petkit.TalkbackDiag()
	fmt.Println("============================")

	// File mode: first arg is a path to an .aac file.
	if len(os.Args) > 1 && strings.HasSuffix(os.Args[1], ".aac") {
		if err := petkit.SelfTestFile(os.Args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Println("done")
		return
	}

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
