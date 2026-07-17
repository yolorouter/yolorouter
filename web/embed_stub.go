//go:build !embed

package web

import "embed"

// DistFS is the zero-value embed.FS (no files) for every build that
// doesn't pass -tags embed — plain `go build`/`go vet`/`go test`, and
// anything an IDE/gopls runs in the background. This is what lets
// web/dist/ stay 100% gitignored with zero tracked exceptions: nothing
// requires it to contain anything unless the embed tag is set (see
// embed_real.go for the full list of what sets it).
//
// router.New() never assumes DistFS actually has a "dist" entry — every
// static-file lookup goes through isRegularFile's fs.Stat call, which
// correctly reports "not found" against this zero-value FS for any path,
// so requests fall back to PlaceholderHTML without any special-casing.
var DistFS embed.FS
