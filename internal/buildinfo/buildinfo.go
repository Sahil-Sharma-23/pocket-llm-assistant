// Package buildinfo exposes version metadata injected at link time.
//
// Build with:
//
//	go build -ldflags "-X personal-assistant/internal/buildinfo.Version=v0.1.0 \
//	  -X personal-assistant/internal/buildinfo.Commit=$(git rev-parse --short HEAD)"
package buildinfo

import "runtime"

// These are overridden via -ldflags -X at build time.
var (
	Version = "dev"
	Commit  = "unknown"
)

// Info is a snapshot of build and runtime platform metadata.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get returns the current build info.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}
