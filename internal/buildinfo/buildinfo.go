// Package buildinfo carries the version metadata stamped in at link time.
//
// It is a leaf package so that both the CLI and the metrics registry can read
// the same values without either importing the other.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Values are overwritten by main via Set, which receives them from -ldflags.
// The defaults are what a plain `go run` reports.
var (
	version   = "dev"
	commit    = "none"
	date      = "unknown"
	goVersion = runtime.Version()
)

// Set records the link-time values. main calls it once, before anything else.
func Set(v, c, d string) {
	if v != "" {
		version = v
	}
	if c != "" {
		commit = c
	}
	if d != "" {
		date = d
	}
	// A `go install`ed binary has no -ldflags but does have VCS stamps.
	if commit == "none" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					if len(s.Value) >= 7 {
						commit = s.Value[:7]
					}
				case "vcs.time":
					if date == "unknown" {
						date = s.Value
					}
				}
			}
		}
	}
}

// Info is the full set, for /version and the build_info metric.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns the current build metadata.
func Get() Info {
	return Info{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: goVersion,
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// Version is the short form used in log records and the User-Agent.
func Version() string { return version }

// String is the one-line form printed by `taskapi version`.
func (i Info) String() string {
	return fmt.Sprintf("taskapi %s (commit %s, built %s, %s, %s)",
		i.Version, i.Commit, i.Date, i.GoVersion, i.Platform)
}
