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
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/gateway"
	"github.com/yolorouter/yolorouter-ce/internal/handler"
	"github.com/yolorouter/yolorouter-ce/internal/middleware"
	"github.com/yolorouter/yolorouter-ce/internal/service"
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
// / web/embed_stub.go). providerMasterKey is the already-decoded 32-byte
// AES-256-GCM key (cmd/yolorouter-ce/serve.go decodes it via
// crypto.KeyFromBase64 before calling this) — passed here rather than read
// from a global so provider_service.go's dependencies stay explicit, same
// as db.
// bodiesDir is the absolute data/bodies/ directory (already created by
// cmd/yolorouter-ce/serve.go at boot) that the gateway's stream body
// capture (internal/gateway/stream.go) appends sent-SSE files under. The
// gateway package has no direct access to app config — passing the
// resolved absolute path down through New/newWithDistFS and stashing it on
// every request's gin.Context (below) is how it crosses that boundary
// without an import cycle.
func New(db *gorm.DB, providerMasterKey []byte, bodiesDir string) (*gin.Engine, error) {
	// fs.Sub never actually errors here, in either build variant: it only
	// validates that "dist" is a syntactically-valid path string, not that
	// it exists in web.DistFS (confirmed against io/fs's Sub implementation
	// — embed.FS doesn't implement fs.SubFS, so this falls into the
	// generic wrapping path, which doesn't check existence). The real
	// gating against a plain build's empty web.DistFS is isRegularFile's
	// fs.Stat call at each call site below, which correctly reports
	// "not found" for every path against an empty embedded FS.
	distFS, _ := fs.Sub(web.DistFS, "dist")
	return newWithDistFS(distFS, db, providerMasterKey, bodiesDir)
}

func newWithDistFS(distFS fs.FS, db *gorm.DB, providerMasterKey []byte, bodiesDir string) (*gin.Engine, error) {
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

	if err := handler.RegisterValidators(); err != nil {
		return nil, fmt.Errorf("register validators: %w", err)
	}

	admin := r.Group("/api/admin")
	admin.Use(middleware.BodySizeLimit(1 << 20)) // 1MiB, per design doc §5 / M0 §8 limit

	// Public auth routes — the only /api/admin endpoints that don't require
	// a session. Every other route on this group, including every future
	// module's, registers on the protected subgroup below instead of
	// directly on admin, so a route missing RequireAdminSession is a
	// deliberate exception the reviewer has to notice, not a silent
	// default that a forgotten middleware call slips through.
	//
	// loginConcurrencyLimit caps the number of in-flight bcrypt
	// comparisons PostLogin can trigger at once — an unknown username
	// still runs a full bcrypt comparison (see
	// internal/service/auth_service.go's dummyPasswordHashForTiming,
	// added to close an account-enumeration timing side channel), and the
	// per-account lockout can't apply to a username with no matching row.
	// See middleware.Semaphore's doc comment for why PostLogin acquires
	// this itself around just the service.Login call, rather than this
	// being a middleware wrapping the whole handler (including the
	// request body read).
	const loginConcurrencyLimit = 8
	loginLimiter := middleware.NewSemaphore(loginConcurrencyLimit)
	admin.GET("/auth/state", handler.GetAuthState(db))
	admin.POST("/auth/setup", handler.PostSetup(db))
	admin.POST("/auth/login", handler.PostLogin(db, loginLimiter))

	protected := admin.Group("")
	protected.Use(middleware.RequireAdminSession(db))
	protected.POST("/auth/logout", handler.PostLogout(db))
	protected.GET("/auth/me", handler.GetMe(db))
	protected.PUT("/auth/password", handler.PutPassword(db))

	providerSvc := service.NewProviderService(db, providerMasterKey, service.NewHTTPProviderClient())
	protected.GET("/providers", handler.GetProviders(providerSvc))
	protected.POST("/providers", handler.PostProvider(providerSvc))
	protected.POST("/providers/test-key", handler.PostProviderTestKey(providerSvc))
	protected.GET("/providers/:id", handler.GetProvider(providerSvc))
	protected.PATCH("/providers/:id", handler.PatchProvider(providerSvc))
	protected.PATCH("/providers/:id/status", handler.PatchProviderStatus(providerSvc))
	protected.POST("/providers/:id/keys", handler.PostProviderKey(providerSvc))
	protected.PATCH("/providers/:id/keys/:keyId", handler.PatchProviderKey(providerSvc))
	protected.PATCH("/providers/:id/keys/:keyId/order", handler.PatchProviderKeyOrder(providerSvc))
	protected.PATCH("/providers/:id/keys/:keyId/status", handler.PatchProviderKeyStatus(providerSvc))
	protected.POST("/providers/:id/keys/:keyId/test", handler.PostProviderKeyTest(providerSvc))
	protected.POST("/providers/:id/keys/test-all", handler.PostProviderKeysTestAll(providerSvc))

	modelSvc := service.NewModelService(db, providerMasterKey, service.NewHTTPProviderClient())
	protected.GET("/models", handler.GetModels(modelSvc))
	protected.POST("/models", handler.PostModel(modelSvc))
	protected.GET("/models/:id", handler.GetModel(modelSvc))
	protected.PATCH("/models/:id", handler.PatchModel(modelSvc))
	protected.PATCH("/models/:id/status", handler.PatchModelStatus(modelSvc))
	protected.POST("/models/:id/candidates/test-mapping", handler.PostModelCandidateTestMapping(modelSvc))
	protected.POST("/models/:id/candidates", handler.PostModelCandidate(modelSvc))
	protected.PATCH("/models/:id/candidates/:candidateId", handler.PatchModelCandidate(modelSvc))
	protected.PATCH("/models/:id/candidates/:candidateId/order", handler.PatchModelCandidateOrder(modelSvc))
	protected.PATCH("/models/:id/candidates/:candidateId/status", handler.PatchModelCandidateStatus(modelSvc))
	protected.POST("/models/:id/candidates/:candidateId/test", handler.PostModelCandidateTest(modelSvc))
	protected.DELETE("/models/:id/candidates/:candidateId", handler.DeleteModelCandidate(modelSvc))

	apiKeySvc := service.NewAPIKeyService(db)
	protected.GET("/api-keys", handler.GetAPIKeys(apiKeySvc))
	protected.POST("/api-keys", handler.PostAPIKey(apiKeySvc))
	protected.GET("/api-keys/:id", handler.GetAPIKey(apiKeySvc))
	protected.PATCH("/api-keys/:id", handler.PatchAPIKey(apiKeySvc))
	protected.PATCH("/api-keys/:id/revoke", handler.PatchAPIKeyRevoke(apiKeySvc))

	// M6.1: dashboard / analytics / request logs (PRD §6.6 / §6.7 / §6.8).
	// All three are read-only queries over request_logs (written by the M5
	// gateway), layered handler → service → repository like M1-M4. The
	// /request-logs/export route MUST be registered before /request-logs/:requestId
	// or gin treats "export" as a requestId.
	dashboardSvc := service.NewDashboardService(db)
	protected.GET("/dashboard", handler.GetDashboard(dashboardSvc))

	analyticsSvc := service.NewAnalyticsService(db)
	protected.GET("/analytics/overview", handler.GetAnalyticsOverview(analyticsSvc))
	protected.GET("/analytics/report", handler.GetAnalyticsReport(analyticsSvc))
	protected.GET("/analytics/export", handler.ExportAnalyticsCSV(analyticsSvc))

	requestLogSvc := service.NewRequestLogService(db)
	protected.GET("/request-logs", handler.GetRequestLogs(requestLogSvc))
	protected.GET("/request-logs/export", handler.ExportRequestLogsCSV(requestLogSvc))
	protected.GET("/request-logs/:requestId", handler.GetRequestLogDetail(requestLogSvc))
	protected.GET("/request-logs/:requestId/body/stream", handler.GetRequestLogBodyStream(requestLogSvc, bodiesDir))

	// Gateway: POST /v1/chat/completions — the second auth path (PRD §6.5).
	// The caller presents an API key in Authorization: Bearer, not a session
	// cookie. The 20MiB body cap is M0's gateway limit (design doc §5/§8),
	// larger than the admin JSON API's 1MiB to leave room for long histories
	// and tool definitions.
	relaySvc := gateway.NewRelayService(db, providerMasterKey)
	v1 := r.Group("/v1", middleware.BodySizeLimit(20<<20), middleware.APIKeyAuth(db))
	// M6.2: stash the absolute bodies dir on the request context so the
	// gateway package (which cannot import app config without a cycle) can
	// resolve where to append its stream capture file via
	// c.GetString("bodies_dir") — see internal/gateway/stream.go's
	// streamBodiesDir.
	v1.Use(func(c *gin.Context) {
		c.Set("bodies_dir", bodiesDir)
		c.Next()
	})
	v1.POST("/chat/completions", gateway.PostChatCompletions(relaySvc))

	return r, nil
}

// isStaticAssetNamespace reports whether path falls under the embedded
// frontend's hashed static-asset directory (Vite's `assets/` build output,
// design doc §7). Unlike arbitrary SPA client routes, a miss here is a real
// 404, not an index.html fallback.
func isStaticAssetNamespace(path string) bool {
	return path == "/assets" || strings.HasPrefix(path, "/assets/")
}
