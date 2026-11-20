// Package providers carries the engine adapter files inside the ezbx program,
// so the finished binary works without any files beside it.
package providers

import "embed"

// FS holds every providers/<id>/adapter.json file.
//
//go:embed */adapter.json
var FS embed.FS
