package web

import _ "embed"

// PlaceholderHTML is served by the router's SPA fallback whenever DistFS is
// empty — every plain (non -tags embed) build; see embed_stub.go. A -tags
// embed build can never reach this fallback: an empty dist/ fails to
// compile (embed_real.go), and a populated dist/ missing index.html fails
// router.New() at startup instead of serving this placeholder (see
// validateEmbeddedFrontend in internal/router/router.go). Unlike DistFS,
// this file has no build tag: placeholder.html is a small,
// permanently-tracked file that never depends on web/dist/ having
// anything, so embedding it always compiles.
//
//go:embed placeholder.html
var PlaceholderHTML []byte
