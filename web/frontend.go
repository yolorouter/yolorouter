package web

import "io/fs"

// HasFrontend reports whether DistFS actually carries an embedded frontend
// build: true for every -tags embed build (an empty dist/ under that tag
// fails to compile — see embed_real.go — so this is always true once the
// binary exists), false for every plain build (embed_stub.go's zero-value
// DistFS has no "dist" entry at all).
func HasFrontend() bool {
	entries, err := fs.ReadDir(DistFS, "dist")
	return err == nil && len(entries) > 0
}
