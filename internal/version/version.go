// Package version holds build metadata injected with -ldflags.
package version

import (
	"fmt"
	"runtime"
)

// Overridden at build time:
//
//	go build -ldflags "-X github.com/jahrulnr/fakeagents/internal/version.Version=v1.0.0"
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns a human readable version line.
func String() string {
	return fmt.Sprintf("fakeagents %s (commit %s, built %s, %s)", Version, Commit, Date, runtime.Version())
}
