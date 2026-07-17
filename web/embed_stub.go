//go:build !embed

package web

import "embed"

// DistFS is the zero-value embed.FS (no files) for every build that
// doesn't pass -tags embed — plain `go build`/`go vet`/`go test`, and
// anything an IDE/gopls runs in the background. This is what lets
// web/dist/ stay 100% gitignored with zero tracked exceptions: nothing
// requires it to contain anything unless the embed tag is set, and only
// scripts/dev.sh and `make build-release` ever set that tag, both of which
// guarantee dist/ has a real frontend build on disk first (see their
// -tags embed / -tags release,embed build steps).
//
// router.New() checks fs.Sub(DistFS, "dist") for an error rather than
// assuming it always succeeds, so this zero-value FS (which has no "dist"
// entry at all) doesn't panic — it just falls back to PlaceholderHTML for
// every request.
var DistFS embed.FS
