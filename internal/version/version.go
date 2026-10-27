// Package version reports build information for the alixai binary.
//
// Release builds stamp Version, Commit and Date with
//
//	-ldflags "-X github.com/buxee/alix-ai/internal/version.Version=1.2.3"
//
// Builds without stamping fall back to the VCS data Go embeds in the binary.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// ReleaseLine is the current product release this source tree prepares for.
// Unstamped development binaries report ReleaseLine+"-dev" as the package
// compatibility core version (see apppkg.DevCoreVersion) so packages written
// for [1.0.0, 2.0.0) load under both stamped releases and source builds.
const ReleaseLine = "1.5.0"

// Stamped at link time. The zero values mean "not stamped".
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// Info is the resolved build information.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	Modified  bool   `json:"modified"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get resolves the build information. Linker-stamped values win; missing ones
// are filled from debug.ReadBuildInfo; anything still unknown is reported as
// "dev" (version) or "unknown" (commit, date).
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = s.Value
				}
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.Date == "" {
		info.Date = "unknown"
	}
	return info
}

// String renders the build information on one line.
func (i Info) String() string {
	commit := i.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if i.Modified {
		commit += "+dirty"
	}
	return fmt.Sprintf("alixai %s (commit %s, built %s, %s %s/%s)", i.Version, commit, i.Date, i.GoVersion, i.OS, i.Arch)
}

// String is shorthand for Get().String().
func String() string { return Get().String() }
