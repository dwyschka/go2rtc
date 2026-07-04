//go:build !linux

package petkit

import (
	"errors"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

// Dial is only implemented on Linux — the petkit driver reads the camera's
// shared-memory media ring and must run on the device itself.
func Dial(source string) (core.Producer, error) {
	if _, err := parseSource(source); err != nil {
		return nil, err
	}
	return nil, errors.New("petkit: shared-memory source is only supported on the device (linux)")
}
