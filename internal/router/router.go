// Package router wires up the Gin engine: health check, embedded frontend
// static assets with SPA fallback, and the /api|/v1 namespace 404 dispatch.
package router

import (
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/internal/middleware"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
	"github.com/yolorouter/yolorouter-ce/web"
)

// isRegularFile reports whether name exists in fsys and is a regular file,
// not a directory — a real Vite build has an assets/ directory, and serving
// a directory path via http.ServeFileFS would list its contents instead of
// falling through to isStaticAssetNamespace's real-404 branch below.
func isRegularFile(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

// hasAnyFile reports whether fsys contains at least one entry at its root.
// Used to distinguish "no frontend build embedded at all" from "a frontend
// build was embedded but it's missing index.html" (a broken build — e.g. a
// Vite output-path misconfiguration that still exits 0). In practice this
// can only be false for a plain (non -tags embed) build — see
// embed_stub.go — since a -tags embed build with an empty dist/ fails to
// compile in the first place (embed_real.go), so that state never reaches
// a running binary at all.
func hasAnyFile(fsys fs.FS) bool {
	entries, err := fs.ReadDir(fsys, ".")
	return err == nil && len(entries) > 0
}

// localAssetRefPattern matches root-relative src/href references in
// index.html, e.g. `src="/assets/index-CNWoupNg.js"` or
// `href="/assets/index-DheEHt3s.css"` — Vite's actual build output always
// references its own hashed assets this way. Anything not starting with
// "/" (an external https:// URL, a bare "#" anchor, etc.) is deliberately
// left unmatched; there is nothing local to check it against.
var localAssetRefPattern = regexp.MustCompile(`(?:src|href)="(/[^"]+)"`)

// validateEmbeddedFrontend enforces the dist/index.html invariant at
// startup rather than leaving it to be discovered per-request: a populated
// distFS (any -tags embed/release,embed build that actually ran the
// frontend build step) must have a non-empty index.html whose referenced
// local assets actually exist, or the embedded build is broken. An empty
// distFS (no frontend embedded at all) is fine — that's the expected
// placeholder case.
//
// This must run before New() returns, not just be handled as a per-request
// fallback in NoRoute: /healthz is a separate, unconditionally-registered
// route that never goes through NoRoute at all, so a broken embed would
// otherwise still report healthy while every real page request 500s —
// invisible to any health/readiness check, and a broken deploy could stay
// "Ready" indefinitely. Failing New() itself means the process never
// starts serving traffic in that state, so the deployment fails loudly at
// startup instead.
func validateEmbeddedFrontend(distFS fs.FS) error {
	if !hasAnyFile(distFS) {
		return nil
	}
	if !isRegularFile(distFS, "index.html") {
		return fmt.Errorf("embedded frontend build is broken: web/dist/ has files but no index.html (a Vite output-path misconfiguration?)")
	}
	indexHTML, err := fs.ReadFile(distFS, "index.html")
	if err != nil {
		return fmt.Errorf("embedded frontend build is broken: cannot read index.html: %w", err)
	}
	if len(indexHTML) == 0 {
		return fmt.Errorf("embedded frontend build is broken: index.html is empty")
	}
	for _, match := range localAssetRefPattern.FindAllSubmatch(indexHTML, -1) {
		assetPath := strings.TrimPrefix(string(match[1]), "/")
		if assetPath == "" {
			continue
		}
		if !isRegularFile(distFS, assetPath) {
			return fmt.Errorf("embedded frontend build is broken: index.html references %q, which is missing from the embedded build", match[1])
		}
	}
	return nil
}

// New builds the router against the real embedded frontend (web.DistFS,
// selected at compile time by the embed build tag — see web/embed_real.go
// / web/embed_stub.go). Thin wrapper around newWithDistFS so tests can
// exercise the actual routing/validation logic against an injected fake
// FS instead — embed.FS values can only ever come from a real go:embed
// directive, so there's no other way to construct a "populated but
// missing index.html" filesystem to test validateEmbeddedFrontend's
// integration with New() end-to-end.
func New() (*gin.Engine, error) {
	// fs.Sub never actually errors here, in either build variant: it only
	// validates that "dist" is a syntactically-valid path string, not that
	// it exists in web.DistFS (confirmed against io/fs's Sub implementation
	// — embed.FS doesn't implement fs.SubFS, so this falls into the
	// generic wrapping path, which doesn't check existence). The real
	// gating against a plain build's empty web.DistFS is isRegularFile's
	// fs.Stat call at each call site below, which correctly reports
	// "not found" for every path against an empty embedded FS.
	distFS, _ := fs.Sub(web.DistFS, "dist")
	return newWithDistFS(distFS)
}

func newWithDistFS(distFS fs.FS) (*gin.Engine, error) {
	if err := validateEmbeddedFrontend(distFS); err != nil {
		return nil, err
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true

	r.Use(middleware.RequestID())
	r.Use(middleware.AccessLog())
	r.Use(middleware.Recovery())

	healthz := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	r.GET("/healthz", healthz)
	r.HEAD("/healthz", healthz) // design doc §10/§14 criterion 7: /healthz accepts GET and HEAD

	// NoMethod covers a wrong-method request against an already-registered
	// route (e.g. POST /healthz); without this, Gin's built-in NoMethod
	// handler would answer with a plain-text 405 instead of a unified
	// envelope. It must still dispatch by namespace the same way NoRoute
	// does below — a wrong-method /v1/* request must get the OpenAI-style
	// shape, not the admin envelope.
	r.NoMethod(func(c *gin.Context) {
		middleware.WriteNamespacedError(c, c.Request.URL.Path, http.StatusMethodNotAllowed, errcode.MethodNotAllowed)
	})

	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path

		if middleware.IsAdminNamespace(path) || middleware.IsGatewayNamespace(path) {
			middleware.WriteNamespacedError(c, path, http.StatusNotFound, errcode.RouteNotFound)
			return
		}

		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			middleware.WriteAdminError(c, http.StatusMethodNotAllowed, errcode.MethodNotAllowed)
			return
		}

		assetPath := strings.TrimPrefix(path, "/")
		if assetPath == "" {
			assetPath = "index.html"
		}
		if isRegularFile(distFS, assetPath) {
			http.ServeFileFS(c.Writer, c.Request, distFS, assetPath)
			return
		}

		// Requests under the hashed static-asset directory (Vite's
		// build convention, design doc §7) are real asset lookups, not
		// SPA client routes — a miss here must be a genuine 404, or a
		// stale/incorrect asset reference would silently "succeed" by
		// serving index.html instead of surfacing as broken.
		if isStaticAssetNamespace(path) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		// SPA fallback: no matching embedded file, hand off to the
		// frontend router. validateEmbeddedFrontend above already
		// guarantees that if distFS has any content, index.html exists —
		// so reaching here with isRegularFile(distFS, "index.html") false
		// means distFS is genuinely empty (no frontend embedded at all),
		// not a broken build; serving the placeholder is correct.
		c.Header("Cache-Control", "no-cache")
		if isRegularFile(distFS, "index.html") {
			http.ServeFileFS(c.Writer, c.Request, distFS, "index.html")
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", web.PlaceholderHTML)
	})

	return r, nil
}

// isStaticAssetNamespace reports whether path falls under the embedded
// frontend's hashed static-asset directory (Vite's `assets/` build output,
// design doc §7). Unlike arbitrary SPA client routes, a miss here is a real
// 404, not an index.html fallback.
func isStaticAssetNamespace(path string) bool {
	return path == "/assets" || strings.HasPrefix(path, "/assets/")
}
