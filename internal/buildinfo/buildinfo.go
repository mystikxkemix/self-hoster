// Package buildinfo reports which version of self-hoster is running.
package buildinfo

import (
	"regexp"
	"runtime/debug"
)

// version is set at build time:
//
//	go build -ldflags "-X selfhoster/internal/buildinfo.version=1.42"
//
// The Dockerfile does this with the VERSION build arg. CI sets it to
// 2.<number of merges/pushes on master>, the same value as the image tag.
var version string

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Version returns the build version: the CI version (e.g. "1.42"), else the short
// git commit of a local build, else "dev".
func Version() string {
	v := version
	if v == "" {
		v = vcsRevision()
	}
	return format(v)
}

// vcsRevision is the commit Go records when building inside a git checkout
// (e.g. `go build` or `go run` in the dev container). Docker builds have no .git.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	revision, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision != "" && modified {
		return format(revision) + "-dirty"
	}
	return revision
}

// format shortens a full commit hash to 7 characters, like GitHub.
func format(v string) string {
	switch {
	case v == "":
		return "dev"
	case fullSHA.MatchString(v):
		return v[:7]
	default:
		return v
	}
}
