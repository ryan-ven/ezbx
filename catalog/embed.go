// Package catalog carries the tool catalogue inside the ezbx program: one
// file per tool (tools/<id>.json) and one per tool pack (packs/<id>.json).
// See docs/build.md.
package catalog

import "embed"

// FS holds every tools/*.json and packs/*.json file.
//
//go:embed tools/*.json packs/*.json
var FS embed.FS
