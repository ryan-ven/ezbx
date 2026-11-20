// Package harnesses carries the harness manifests inside the ezbx program,
// so the finished binary works without any files beside it.
package harnesses

import "embed"

// FS holds every harnesses/<id>/manifest.json file.
//
//go:embed */manifest.json
var FS embed.FS
