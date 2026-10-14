// Package buildinfo exposes the server version and commit embedded at build
// time via -ldflags, with a fallback to the VCS info Go records itself.
package buildinfo

import (
	"runtime/debug"
	"strings"
	"sync"
)

// Set via -ldflags "-X github.com/LNA-DEV/ytdlp-nfo-server/internal/buildinfo.Version=..."
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

var once sync.Once

func load() {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "" {
				Commit = s.Value
			}
		case "vcs.time":
			if Date == "" {
				Date = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				defer func() {
					if Commit != "" && !strings.HasSuffix(Commit, "-dirty") {
						Commit += "-dirty"
					}
				}()
			}
		}
	}
	if Commit == "" {
		Commit = "unknown"
	}
}

// Info returns version, short commit and build date.
func Info() (version, commit, date string) {
	once.Do(load)
	c := strings.TrimSuffix(Commit, "-dirty")
	if len(c) > 12 {
		c = c[:12]
	}
	if strings.HasSuffix(Commit, "-dirty") {
		c += "-dirty"
	}
	return Version, c, Date
}

// String returns a one-line description such as "v2.0.0 (commit abc123)".
func String() string {
	v, c, d := Info()
	s := v + " (commit " + c
	if d != "" {
		s += ", built " + d
	}
	return s + ")"
}
