package main

import (
	"slices"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/pkg/shell"
)

// module is one initializable go2rtc subsystem. The concrete list is provided
// by modules_full.go (default) or modules_min.go (built with -tags petkit_min).
type module struct {
	name string
	init func()
}

func main() {
	// version will be set later from -buildvcs info, this used only as fallback
	app.Version = "1.9.14"

	for _, m := range activeModules() {
		if app.Modules == nil || m.name == "" || slices.Contains(app.Modules, m.name) {
			m.init()
		}
	}

	shell.RunUntilSignal()
}
