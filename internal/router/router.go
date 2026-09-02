// Package router wires up the Gin engine: health check, embedded frontend
// static assets with SPA fallback, and the /api|/v1 namespace 404 dispatch.
package router

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/config"
	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/handler"
	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/selfupdate"
	"github.com/yolorouter/yolorouter/internal/service/analytics"
	"github.com/yolorouter/yolorouter/internal/service/apikey"
	"github.com/yolorouter/yolorouter/internal/service/dashboard"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/oauth"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/service/requestlog"
	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
	versionsvc "github.com/yolorouter/yolorouter/internal/service/version"
	"github.com/yolorouter/yolorouter/internal/version"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/web"
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

// Deps carries everything the router assembly needs. One struct instead of a
// positional list: several of these dependencies share a type, and a list
// that long is a silent-swap waiting to happen at every call site.
type Deps struct {
	DB *gorm.DB
	// ProviderMasterKey is the already-decoded 32-byte AES-256-GCM key
	// (cmd/yolorouter/serve.go decodes it via crypto.KeyFromBase64 before
	// calling New) — passed here rather than read from a global so
	// the provider service's dependencies stay explicit, same as DB.
	ProviderMasterKey []byte
	// BodiesDir is the absolute data/bodies/ directory (already created by
	// cmd/yolorouter/serve.go at boot) that the gateway's stream body capture
	// appends sent-SSE files under. The gateway package has no direct access
	// to app config — passing the resolved absolute path down and stashing it
	// on every request's gin.Context is how it crosses that boundary without
	// an import cycle.
	BodiesDir string
	// Update carries the version-update settings the system-info endpoint
	// resolves its GitHub release source from.
	Update config.UpdateConfig
	// AllowPrivateUpstreams (config.SecurityConfig.AllowPrivateUpstreams) is
	// forwarded to the provider-test and gateway-relay clients' SSRF
	// transport, letting a self-hosted operator reach a LAN/localhost model
	// server.
	AllowPrivateUpstreams bool
	// ProbeQueue receives the candidates a bulk import stores, for background
	// verification. The server owns its lifecycle (start/stop with serve's own
	// context) and passes it in; a nil queue — router tests that never
	// exercise imports — simply skips enqueueing.
	ProbeQueue *modeladmin.ProbeQueue
	// Gateway carries the relay timeouts and limits, threaded through so the
	// wiring stays identical to production instead of a zero struct.
	Gateway config.GatewayConfig
	// LoopbackBase is the server's own base URL, which the gateway's
	// vision-fallback capability calls back into.
	LoopbackBase string
	// ExternalURL is the public base URL OAuth providers redirect back to.
	ExternalURL string
}

// New builds the router against the real embedded frontend (web.DistFS,
// selected at compile time by the embed build tag — see web/embed_real.go
// / web/embed_stub.go).
func New(deps Deps) (*gin.Engine, error) {
	// fs.Sub never actually errors here, in either build variant: it only
	// validates that "dist" is a syntactically-valid path string, not that
	// it exists in web.DistFS (confirmed against io/fs's Sub implementation
	// — embed.FS doesn't implement fs.SubFS, so this falls into the
	// generic wrapping path, which doesn't check existence). The real
	// gating against a plain build's empty web.DistFS is isRegularFile's
	// fs.Stat call at each call site below, which correctly reports
	// "not found" for every path against an empty embedded FS.
	distFS, _ := fs.Sub(web.DistFS, "dist")
	return newWithDistFS(distFS, deps)
}

func newWithDistFS(distFS fs.FS, deps Deps) (*gin.Engine, error) {
	db, secrets := deps.DB, crypto.NewSecretBox(deps.ProviderMasterKey)
	bodiesDir, updateCfg := deps.BodiesDir, deps.Update
	allowPrivateUpstreams, gatewayCfg := deps.AllowPrivateUpstreams, deps.Gateway
	loopbackBase, externalURL := deps.LoopbackBase, deps.ExternalURL
	if err := validateEmbeddedFrontend(distFS); err != nil {
		return nil, err
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true

	r.Use(middleware.RequestID())
	r.Use(middleware.AccessLog())
	r.Use(middleware.Recovery())
	r.Use(middleware.Timezone())

	healthz := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
	r.GET("/healthz", healthz)
	r.HEAD("/healthz", healthz) // /healthz accepts GET and HEAD

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
		// build convention) are real asset lookups, not
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
	admin.Use(middleware.BodySizeLimit(1 << 20)) // 1MiB limit

	// Public auth routes — the only /api/admin endpoints that don't require
	// a session. Every other route on this group, including every future
	// module's, registers on the protected subgroup below instead of
	// directly on admin, so a route missing RequireSession is a
	// deliberate exception the reviewer has to notice, not a silent
	// default that a forgotten middleware call slips through.
	//
	// loginConcurrencyLimit caps the number of in-flight bcrypt
	// comparisons PostLogin can trigger at once — an unknown username
	// still runs a full bcrypt comparison (see
	// internal/service/auth's dummyPasswordHashForTiming,
	// added to close an account-enumeration timing side channel), and the
	// per-account lockout can't apply to a username with no matching row.
	// See middleware.Semaphore's doc comment for why PostLogin acquires
	// this itself around just the auth.Login call, rather than this
	// being a middleware wrapping the whole handler (including the
	// request body read).
	const loginConcurrencyLimit = 8
	loginLimiter := middleware.NewSemaphore(loginConcurrencyLimit)
	admin.GET("/auth/state", handler.GetAuthState(db))
	admin.POST("/auth/setup", handler.PostSetup(db))
	admin.POST("/auth/login", handler.PostLogin(db, loginLimiter))

	// External-login flow. The provider list and state issuance are public
	// by necessity (the login page shows them before any session exists);
	// both expose nothing beyond what the login page must render, and the
	// browser-facing callback lives at the root (it is a navigation target
	// the identity provider redirects to, not an API call).
	oauthLoginSvc := oauth.NewOAuthLoginService(db, secrets)
	admin.GET("/auth/oauth/providers", handler.GetPublicOAuthProviders(oauthLoginSvc))
	// Rate shape: a generous global ceiling (600/min) so the endpoint can
	// never grow auth_states unboundedly, plus a per-client budget
	// (20/min) so one caller exhausting its budget cannot starve everyone
	// else's login window.
	admin.POST("/auth/oauth/state", handler.PostOAuthState(oauthLoginSvc,
		middleware.NewSemaphore(loginConcurrencyLimit),
		middleware.NewRateWindow(600, time.Minute),
		middleware.NewPerClientRateWindow(20, time.Minute), externalURL))
	r.GET("/oauth/callback/:slug", handler.GetOAuthCallback(oauthLoginSvc, middleware.NewSemaphore(loginConcurrencyLimit)))

	// Self-scoped routes: any signed-in account (admin or member) may read
	// its own identity, end its own session, and change its own password —
	// these carry no cross-account reach by construction.
	sessionOnly := admin.Group("")
	sessionOnly.Use(middleware.RequireSession(db))
	sessionOnly.POST("/auth/logout", handler.PostLogout(db))
	sessionOnly.GET("/auth/me", handler.GetMe(db))
	sessionOnly.PUT("/auth/password", handler.PutPassword(db))
	// The gateway address is readable by any signed-in account, unlike the
	// admin-only build info above — GetSystemEndpoint owns the rationale.
	sessionOnly.GET("/system/endpoint", handler.GetSystemEndpoint(externalURL))

	// Ownership-scoped routes: reachable by members, but every query a
	// non-admin makes through them is pinned to their own rows by
	// MemberScope + the ViewScope checks in the handlers/services —
	// list filters overridden, by-id operations owner-checked, provider
	// dimensions and deployment sections refused. Admins pass through
	// with full reach. Registration happens below once the services exist.
	scoped := admin.Group("")
	scoped.Use(middleware.RequireSession(db), middleware.MemberScope())

	// Every route below requires a valid session AND the admin role. When
	// more member-visible routes arrive they will register on the
	// session-only subgroup above — the admin requirement stays the default
	// so that forgetting to classify a new route locks it down rather than
	// opening it up.
	protected := admin.Group("")
	protected.Use(middleware.RequireSession(db), middleware.RequireAdmin())
	protected.GET("/users", handler.GetUsers(db))
	protected.POST("/users", handler.PostUser(db))
	protected.POST("/users/:id/password", handler.PostUserPasswordReset(db))
	protected.PATCH("/users/:id/profile", handler.PatchUserProfile(db))
	protected.PATCH("/users/:id/status", handler.PatchUserStatus(db))
	protected.PATCH("/users/:id/role", handler.PatchUserRole(db))

	providerSvc := provider.NewProviderService(db, secrets, providerclient.NewHTTPProviderClient(allowPrivateUpstreams))
	protected.GET("/providers", handler.GetProviders(providerSvc))
	protected.POST("/providers", handler.PostProvider(providerSvc))
	protected.POST("/providers/test-key", handler.PostProviderTestKey(providerSvc))
	protected.POST("/providers/list-models", handler.PostProviderListModels(providerSvc))
	protected.GET("/providers/:id", handler.GetProvider(providerSvc))
	protected.GET("/providers/:id/models", handler.GetProviderListModels(providerSvc))
	protected.GET("/providers/:id/impact", handler.GetProviderImpact(providerSvc))
	protected.PATCH("/providers/:id", handler.PatchProvider(providerSvc))
	protected.PATCH("/providers/:id/status", handler.PatchProviderStatus(providerSvc))
	protected.POST("/providers/:id/keys", handler.PostProviderKey(providerSvc))
	protected.PATCH("/providers/:id/keys/:keyId", handler.PatchProviderKey(providerSvc))
	protected.PATCH("/providers/:id/keys/:keyId/order", handler.PatchProviderKeyOrder(providerSvc))
	protected.PATCH("/providers/:id/keys/:keyId/status", handler.PatchProviderKeyStatus(providerSvc))
	protected.DELETE("/providers/:id/keys/:keyId", handler.DeleteProviderKey(providerSvc))
	protected.DELETE("/providers/:id", handler.DeleteProvider(providerSvc))
	protected.POST("/providers/:id/keys/:keyId/test", handler.PostProviderKeyTest(providerSvc))
	protected.POST("/providers/:id/keys/test-all", handler.PostProviderKeysTestAll(providerSvc))

	modelSvc := modeladmin.NewModelService(db, secrets, providerclient.NewHTTPProviderClient(allowPrivateUpstreams))
	protected.GET("/models", handler.GetModels(modelSvc))
	protected.POST("/models", handler.PostModel(modelSvc))
	protected.POST("/models/batch", handler.PostModelsBatch(modelSvc))
	protected.GET("/models/:id", handler.GetModel(modelSvc))
	protected.GET("/models/:id/impact", handler.GetModelImpact(modelSvc))
	protected.PATCH("/models/:id", handler.PatchModel(modelSvc))
	protected.PATCH("/models/:id/status", handler.PatchModelStatus(modelSvc))
	protected.POST("/models/:id/candidates", handler.PostModelCandidate(modelSvc))
	// suggest-price takes its subject (provider id + upstream model name) from
	// the query string rather than the path, because the candidate it is pricing
	// does not exist yet — there is no :candidateId to scope it to.
	protected.GET("/models/candidates/suggest-price", handler.GetCandidateSuggestPrice(modelSvc))
	protected.POST("/models/:id/candidates/test-and-create", handler.PostModelCandidateTestAndCreate(modelSvc))
	// Bulk import routes live under /providers/:id because their subject is
	// "this provider's upstream catalog", but they are model-domain operations
	// (they create models and candidates), hence the model service.
	protected.POST("/providers/:id/models/import", handler.PostProviderModelsImport(modelSvc, deps.ProbeQueue))
	protected.POST("/providers/:id/models/suggest-prices", handler.PostProviderSuggestPrices(modelSvc))
	protected.GET("/providers/:id/candidates", handler.GetProviderCandidates(modelSvc, deps.ProbeQueue))
	protected.PATCH("/models/:id/candidates/:candidateId", handler.PatchModelCandidate(modelSvc))
	protected.PATCH("/models/:id/candidates/:candidateId/order", handler.PatchModelCandidateOrder(modelSvc))
	protected.PATCH("/models/:id/candidates/:candidateId/status", handler.PatchModelCandidateStatus(modelSvc))
	protected.POST("/models/:id/candidates/:candidateId/test", handler.PostModelCandidateTest(modelSvc))
	protected.DELETE("/models/:id/candidates/:candidateId", handler.DeleteModelCandidate(modelSvc))

	apiKeySvc := apikey.NewAPIKeyService(db, secrets)
	scoped.GET("/api-keys", handler.GetAPIKeys(apiKeySvc))
	scoped.POST("/api-keys", handler.PostAPIKey(apiKeySvc))
	scoped.GET("/api-keys/:id", handler.GetAPIKey(apiKeySvc))
	scoped.GET("/api-keys/:id/plaintext", handler.GetAPIKeyPlaintext(apiKeySvc))
	scoped.PATCH("/api-keys/:id", handler.PatchAPIKey(apiKeySvc))
	scoped.PATCH("/api-keys/:id/revoke", handler.PatchAPIKeyRevoke(apiKeySvc))

	// Custom system prompt (global setting). Read returns the authoritative
	// DB state; PUT uses CAS on version. Registered alongside the other admin
	// resources under the session-protected group.
	settingsSvc := systemsettings.NewSystemSettingsService(db)
	oauthProviderSvc := oauth.NewOAuthProviderService(db, secrets)
	protected.GET("/oauth-providers", handler.GetOAuthProviders(oauthProviderSvc, externalURL))
	protected.POST("/oauth-providers", handler.PostOAuthProvider(oauthProviderSvc))
	protected.PATCH("/oauth-providers/:id", handler.PatchOAuthProvider(oauthProviderSvc))
	protected.DELETE("/oauth-providers/:id", handler.DeleteOAuthProvider(oauthProviderSvc))
	protected.POST("/oauth-providers/discover", handler.PostOAuthDiscover(oauthProviderSvc))

	protected.GET("/system-settings/custom-system-prompt", handler.GetCustomSystemPrompt(settingsSvc))
	protected.PUT("/system-settings/custom-system-prompt", handler.PutCustomSystemPrompt(settingsSvc))
	protected.GET("/system-settings/input-compression", handler.GetInputCompression(settingsSvc))
	protected.PUT("/system-settings/input-compression", handler.PutInputCompression(settingsSvc))
	protected.GET("/system-settings/vision-fallback", handler.GetVisionFallback(settingsSvc))
	protected.PUT("/system-settings/vision-fallback", handler.PutVisionFallback(settingsSvc))

	// Dashboard / analytics / request logs.
	// All three are read-only queries over request_logs (written by the
	// gateway), layered handler → service → repository. The
	// /request-logs/export route MUST be registered before /request-logs/:requestId
	// or gin treats "export" as a requestId.
	dashboardSvc := dashboard.NewDashboardService(db)
	scoped.GET("/dashboard", handler.GetDashboard(dashboardSvc))

	analyticsSvc := analytics.NewAnalyticsService(db)
	scoped.GET("/analytics/overview", handler.GetAnalyticsOverview(analyticsSvc))
	scoped.GET("/analytics/report", handler.GetAnalyticsReport(analyticsSvc))
	scoped.GET("/analytics/export", handler.ExportAnalyticsCSV(analyticsSvc))
	protected.GET("/analytics/compress-stats", handler.GetCompressStats(analyticsSvc))
	protected.GET("/analytics/cache-stats", handler.GetCacheStats(analyticsSvc))
	protected.GET("/analytics/concise-output-projection", handler.GetConciseOutputProjection(analyticsSvc))

	requestLogSvc := requestlog.NewRequestLogService(db)
	protected.GET("/request-logs", handler.GetRequestLogs(requestLogSvc))
	protected.GET("/request-logs/export", handler.ExportRequestLogsCSV(requestLogSvc))
	protected.GET("/request-logs/:requestId", handler.GetRequestLogDetail(requestLogSvc))
	protected.GET("/request-logs/:requestId/body/stream", handler.GetRequestLogBodyStream(requestLogSvc, bodiesDir))

	// M7: System info + update check (GET /api/admin/system/version). Read-only
	// and session-protected like the other admin endpoints. VersionService
	// resolves its repo from updateCfg + the compiled-in default (see
	// version.ResolveRepo); an empty resolved repo disables the check and is
	// surfaced as check_failed, not an error.
	resolvedRepo := version.ResolveRepo(updateCfg.Enabled, updateCfg.GitHubRepo)
	updateMode := selfupdate.Mode(resolvedRepo, version.Version)
	versionSvc := versionsvc.NewVersionService(resolvedRepo, updateCfg.GitHubProxy)
	protected.GET("/system/version", handler.GetSystemVersion(handler.SystemInfo{
		Version:    version.Version,
		Commit:     version.Commit,
		BuildTime:  version.BuildTime,
		GoVersion:  runtime.Version(),
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		DBDriver:   db.Dialector.Name(), //nolint:staticcheck // QF1008 false-positive — gorm.DB exposes the driver name only via Dialector.Name(); there is no db.Name()
		UpdateMode: updateMode,
	}, versionSvc))

	// One-click update (POST /api/admin/system/update): download + verify +
	// replace the binary via the same selfupdate mechanics the CLI uses,
	// then schedule a graceful restart so the service manager brings the new
	// binary up. Session-protected like every other admin endpoint; the
	// updateMode gate inside the handler refuses every runtime where an
	// in-place replacement would be wrong (container, windows, dev build,
	// updates disabled).
	protected.POST("/system/update", handler.PostSystemUpdate(updateMode, func(ctx context.Context) (selfupdate.Result, error) {
		return selfupdate.Apply(ctx, selfupdate.Options{
			Repo:    resolvedRepo,
			Proxy:   updateCfg.GitHubProxy,
			Current: version.Version,
		})
	}, selfupdate.ScheduleRestart))

	// Gateway: POST /v1/chat/completions (OpenAI-compatible),
	// POST /v1/messages (Anthropic-compatible), POST /v1/responses
	// (OpenAI Responses-compatible), and POST /v1beta/models/{model}:{action}
	// (native Gemini generateContent/streamGenerateContent) — the second
	// auth path. The caller presents an API key in Authorization: Bearer or
	// X-Api-Key, not a session cookie. The 20MiB body cap is the gateway
	// limit, larger than the admin JSON API's 1MiB to leave room for long
	// histories and tool definitions. Gemini's native path lives outside
	// /v1, so it's mounted on a sibling /v1beta group via gatewayGroup below
	// rather than bare on r, which would otherwise skip auth, the body-size
	// cap, and the bodies-dir stash. All four routes share the same
	// middleware chain (body-size limit, auth, bodies-dir stash);
	// gateway.PostChatCompletions/Service.Handle dispatch by request
	// path (gateway.IngressProtocol) to pick the caller's actual wire
	// protocol.
	relaySvc := gateway.NewService(db, secrets, allowPrivateUpstreams, settingsSvc, gatewayCfg)
	// The model detail view shows per-candidate sticky-binding counts for
	// balanced models; both sides must read the registry the relay actually
	// routes through, so the gateway's instance is handed over rather than a
	// second one built here.
	modelSvc.SetBindingCounter(relaySvc.Bindings())
	// A retest that PROVES a key works releases the key pool's rate-limit
	// bench — the only reliable recovery signal, since a merely claimed or
	// inconclusive retest proves nothing.
	providerSvc.SetKeyRetestPassedListener(relaySvc.NoteKeyRetestPassed)

	registerCapabilities(relaySvc, db, loopbackBase)

	v1 := gatewayGroup(r, "/v1", bodiesDir, db)
	v1.POST("/chat/completions", gateway.PostChatCompletions(relaySvc))
	v1.POST("/messages", gateway.PostChatCompletions(relaySvc))
	v1.POST("/responses", gateway.PostChatCompletions(relaySvc))
	// Image generation rides the same handler and middleware chain as the
	// chat routes: the path resolves to the images protocol, the modality
	// registry hands the request to the image modality, and everything the
	// chain already does (auth, body cap, budget gate, audit) applies
	// unchanged. The generations request body is small JSON — a prompt, not
	// pixels — so the shared 20MiB cap needs no widening there. The edits
	// route below IS pixels, but its uploads fit the same cap: a reference
	// image is megabytes, not tens of them, and an oversized upload is
	// refused with 413 rather than priced into every other route.
	v1.POST("/images/generations", gateway.PostChatCompletions(relaySvc))
	// The edits path is relative to this /v1 group, unlike the images
	// package's EditPath constant, which is the full ingress route other
	// layers match on (IngressProtocol) and the egress path on
	// OpenAI-compatible providers.
	v1.POST("/images/edits", gateway.PostChatCompletions(relaySvc))
	// Model discovery: GET /v1/models and GET /v1/models/:model are
	// read-only and bypass Service (no provider fan-out, no spend).
	// They reuse the same APIKeyAuth + body-cap chain the relay POSTs above
	// use, so a caller presents the same key as for a completion request.
	v1.GET("/models", gateway.ListModels(db))
	// A catch-all (not ":model") because model ids may be slash-namespaced
	// (deepseek-ai/DeepSeek-V4): net/http decodes "%2F" in URL.Path before
	// gin matches, so a single-segment param can never see such a name. The
	// handler strips the leading "/" gin includes in a catch-all value.
	v1.GET("/models/*model", gateway.RetrieveModel(db))

	v1beta := gatewayGroup(r, "/v1beta", bodiesDir, db)
	// :modelaction captures the whole "{model}:{action}" path segment (a
	// gin path parameter matches everything up to the next "/", colon
	// included); gateway.parseGeminiPath does the actual model/action split
	// once a request reaches the handler.
	//
	// This single-segment param means a model name containing a "/" (a
	// tuned model's "tunedModels/xyz" resource name, percent-encoded as
	// "tunedModels%2Fxyz" in the URL) is unsupported: net/http decodes
	// "%2F" to a literal "/" in URL.Path before gin ever sees it, at which
	// point :modelaction no longer matches the (now two-segment) path at
	// all -- the request 404s here, never reaching parseGeminiPath.
	// Standard (non-tuned) Gemini model names never contain a slash, so
	// this is not a practical gap for this version; see
	// gateway.parseGeminiPath's doc comment and
	// TestGeminiRouteWithSlashInModelSegmentDoesNotRoute (router_test.go)
	// for the full explanation and a routing-layer regression test.
	v1beta.POST("/models/:modelaction", gateway.PostChatCompletions(relaySvc))

	return r, nil
}

// gatewayGroup mounts a caller-facing gateway ingress group at relativePath
// with the middleware chain every gateway route needs: the 20MiB body cap
// (see the comment above this function's call sites), API-key auth (not the
// admin session cookie), and the bodies-dir stash. Factored out so the two
// gateway namespaces (/v1 and /v1beta) can't drift onto different
// middleware chains as more ingress routes are added.
func gatewayGroup(r *gin.Engine, relativePath, bodiesDir string, db *gorm.DB) *gin.RouterGroup {
	g := r.Group(relativePath, middleware.BodySizeLimit(20<<20), middleware.APIKeyAuth(db))
	// Stash the absolute bodies dir on the request context so the
	// gateway package (which cannot import app config without a cycle) can
	// resolve where to append its stream capture file via
	// gateway.BodiesDirContextKey — see internal/gateway/stream.go's
	// streamBodiesDir.
	g.Use(func(c *gin.Context) {
		c.Set(gateway.BodiesDirContextKey, bodiesDir)
		c.Next()
	})
	return g
}

// isStaticAssetNamespace reports whether path falls under the embedded
// frontend's hashed static-asset directory (Vite's `assets/` build output).
// Unlike arbitrary SPA client routes, a miss here is a real
// 404, not an index.html fallback.
func isStaticAssetNamespace(path string) bool {
	return path == "/assets" || strings.HasPrefix(path, "/assets/")
}
