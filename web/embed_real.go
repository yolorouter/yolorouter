//go:build embed

package web

import "embed"

// DistFS embeds the real frontend build for every build tagged with
// -tags embed. Only scripts/dev.sh and `make build-release` set this tag,
// and both guarantee web/dist/ has real content on disk before invoking
// `go build` — the tag exists specifically so no OTHER build path (plain
// `go build`, `go vet`, `go test`, IDE/gopls background analysis) ever
// requires web/dist/ to contain anything, since none of those guarantee
// dist/ has been populated first. Building with this tag against an empty
// dist/ fails to compile ("pattern all:dist: no matching files found") —
// that's intentional: it means the frontend build step was skipped.
//
//go:embed all:dist
var DistFS embed.FS
