// Package ezbx carries EZBox's own build files inside the ezbx program: the
// Dockerfiles, the harness and tool files, and the Go source the gateway
// image is compiled from. So ezbx build works without a copy of the
// repository. See docs/build.md.
package ezbx

import "embed"

// Source holds every file the images are built from. Files whose names start
// with a dot or an underscore are left out, as go:embed always does.
//
//go:embed go.mod go.sum source.go cmd internal providers harnesses catalog docker
var Source embed.FS
