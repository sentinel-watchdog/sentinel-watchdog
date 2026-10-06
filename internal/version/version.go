// Package version exposes build metadata injected at link time:
//
//	go build -ldflags "-X github.com/sentinel-watchdog/sentinel/internal/version.Version=v0.1.0 ..."
package version

import "fmt"

// Values overridden through -ldflags -X at build time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a single-line human readable description of the build.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
}
