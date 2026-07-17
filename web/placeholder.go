package web

import _ "embed"

// PlaceholderHTML is served by the router's SPA fallback whenever DistFS
// has no real index.html — every plain (non -tags embed) build, or a
// -tags embed build where dist/ somehow ended up without one. Unlike
// DistFS, this has no build tag: placeholder.html is a small,
// permanently-tracked file that never depends on web/dist/ having
// anything, so embedding it always compiles.
//
//go:embed placeholder.html
var PlaceholderHTML []byte
