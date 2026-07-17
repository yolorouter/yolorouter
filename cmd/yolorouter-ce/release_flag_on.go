//go:build release

package main

// isReleaseBuild mirrors the db:reset-disabling split between this file's
// pair and db_reset_release.go / db_reset_dev.go. runServe uses it to
// refuse to start when a release binary was built without -tags embed —
// see release_flag_off.go for why this must live in cmd/yolorouter-ce
// rather than the router package.
const isReleaseBuild = true
