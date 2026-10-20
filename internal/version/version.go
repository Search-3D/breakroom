// Package version reports the binary version.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is set at release build time:
//
//	go build -ldflags "-X github.com/search-3d/breakroom/internal/version.Version=1.2.3"
//
// Otherwise it falls back to the module version Go stamps into the binary
// (go install ...@v1.2.3, or a pseudo-version for a VCS checkout), so a
// stale daemon is detectable after any upgrade. The "v" is dropped to match
// release builds, or the same release installed two ways looks stale.
var Version = "0.0.0-dev"

// init falls back to the module version Go stamps into the binary when the
// release build didn't set Version.
func init() {
	if Version != "0.0.0-dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = strings.TrimPrefix(bi.Main.Version, "v")
	}
}
