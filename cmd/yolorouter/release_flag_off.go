//go:build !release

package main

// isReleaseBuild is false for every non -tags release build. Deliberately
// a plain package-level constant, not a check inside internal/router: the
// router package's own tests (both plain and -tags release, via
// newTestRouter -> New()) build against the real web.DistFS, and `make
// vet`/`test-release` intentionally run -tags release without -tags embed
// to exercise db_reset_release.go's release-only code path without
// needing a real frontend build — folding this rule into router.New()
// would fail those every time. Scoping the check to runServe instead (see
// serve.go) is safe because nothing under cmd/yolorouter's test suite
// ever calls runServe.
const isReleaseBuild = false
