// Package handler: external-login (OAuth) endpoints — the public login
// flow and the admin provider management CRUD.
package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/service/auth"
	"github.com/yolorouter/yolorouter/internal/service/oauth"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/logger"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// === Public login flow ====================================================

// GetPublicOAuthProviders handles GET /api/admin/auth/oauth/providers —
// the login page's button list. Public by necessity (shown before any
// session exists) and deliberately minimal: slug, name, icon.
func GetPublicOAuthProviders(svc *oauth.OAuthLoginService) gin.HandlerFunc {
	return func(c *gin.Context) {
		providers, err := svc.ListEnabledProviders()
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		response.Success(c, gin.H{"providers": providers})
	}
}

type oauthStateRequest struct {
	Slug string `json:"slug" binding:"required,max=32"`
}

// oauthStateCookieName carries the SHA-256 of the pending flow's state
// token in the initiating browser. The callback requires it to match the
// state echoed by the provider, which binds the flow to the browser that
// started it: an authorization URL minted by an attacker (login CSRF, or
// a Host-header-poisoned redirect) fails in the victim's browser because
// the victim never held the matching cookie. SameSite=Lax deliberately —
// the callback is a cross-site top-level GET from the provider, which
// Lax permits while still blocking sub-resource/POST leakage.
const oauthStateCookieName = "oauth_state"

func writeOAuthStateCookie(c *gin.Context, value string, maxAge int) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(oauthStateCookieName, value, maxAge, "/oauth/callback", "", false, true)
}

// PostOAuthState handles POST /api/admin/auth/oauth/state — issues the
// one-time state + PKCE pair and returns the full authorization URL. The
// redirect URI comes from the configured external URL when set, else is
// derived from the request's own host (see callbackURL); the exact value
// is pinned into the state row and repeated at token exchange.
func PostOAuthState(svc *oauth.OAuthLoginService, limiter *middleware.Semaphore, rate *middleware.RateWindow, clientRate *middleware.PerClientRateWindow, externalURL string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req oauthStateRequest
		if !bindJSON(c, &req) {
			return
		}
		// Three independent caps on this anonymous endpoint: the global
		// window is the absolute ceiling on rows persisted per minute, the
		// per-client window keeps one caller from starving everyone else,
		// and the semaphore bounds in-flight work — every accepted call
		// stores a state row for its TTL, and concurrency alone doesn't
		// slow a sequential writer down. Order matters: the global check
		// runs first so a flood that already blew the ceiling cannot keep
		// growing the per-client tracking map (its keys come from
		// ClientIP, which is spoofable on deployments that trust proxy
		// headers).
		now := time.Now().UTC()
		if !rate.Allow(now) || !clientRate.Allow(c.ClientIP(), now) || !limiter.TryAcquire() {
			middleware.WriteAdminError(c, http.StatusTooManyRequests, errcode.ServiceUnavailable)
			return
		}
		defer limiter.Release()
		authorizeURL, stateToken, err := svc.BeginLogin(req.Slug, callbackURL(c, req.Slug, externalURL), now)
		if errors.Is(err, errcode.ErrOAuthProviderNotFound) {
			middleware.WriteAdminError(c, http.StatusNotFound, errcode.OAuthProviderNotFound)
			return
		}
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		writeOAuthStateCookie(c, crypto.HashToken(stateToken), int(oauth.AuthStateTTL.Seconds()))
		response.Success(c, gin.H{"authorize_url": authorizeURL})
	}
}

// callbackURL builds this deployment's callback address. When the
// operator configured server.external_url, that trusted origin is used
// verbatim — the right choice for any internet-exposed instance, because
// the fallback derives from the request's Host header and X-Forwarded-
// Proto, both client-controlled on deployments whose proxy does not pin
// them. The derivation fallback is what keeps single-host setups
// (localhost, LAN) zero-config; its residual risk requires the IdP to
// also accept wildcard/dynamic redirect hosts, and the state cookie
// already stops a victim from completing an attacker-minted flow — the
// configured origin closes the remaining replay-by-attacker path.
func callbackURL(c *gin.Context, slug, externalURL string) string {
	return publicBaseURL(c, externalURL) + "/oauth/callback/" + url.PathEscape(slug)
}

// GetOAuthCallback handles GET /oauth/callback/:slug — the browser
// returns here from the identity provider. This is a navigation, not an
// API call, so both outcomes are redirects: success plants the session
// cookie and lands on the app root; failure lands on the login page with
// the error code in the query for the page to translate.
func GetOAuthCallback(svc *oauth.OAuthLoginService, limiter *middleware.Semaphore) gin.HandlerFunc {
	return func(c *gin.Context) {
		slug := c.Param("slug")
		state := c.Query("state")

		// Each callback may spend up to the outbound HTTP timeout talking
		// to the identity provider — bound how many do so at once, or a
		// scripted client could pin goroutines and flood the IdP. Checked
		// before any work; a rejected navigation lands on the login page
		// with a retryable message.
		if !limiter.TryAcquire() {
			c.Redirect(http.StatusFound, "/login?oauth_error="+strconv.Itoa(errcode.ServiceUnavailable))
			return
		}
		defer limiter.Release()

		// Browser binding: the flow must complete in the browser that
		// started it (see oauthStateCookieName). Checked before touching
		// the database so a forged callback costs nothing.
		cookieHash, cookieErr := c.Cookie(oauthStateCookieName)
		if cookieErr != nil || cookieHash == "" || cookieHash != crypto.HashToken(state) {
			logger.Warn("oauth: callback rejected, state cookie missing or mismatched",
				zap.String("request_id", c.GetString(middleware.RequestIDKey)),
				zap.String("slug", slug))
			writeOAuthStateCookie(c, "", -1)
			c.Redirect(http.StatusFound, "/login?oauth_error="+strconv.Itoa(errcode.OAuthStateInvalid))
			return
		}
		// One-shot either way: success or failure, this browser's pending
		// flow is over.
		writeOAuthStateCookie(c, "", -1)

		sessionID, err := svc.HandleCallback(c.Request.Context(), slug, state, c.Query("code"), time.Now().UTC())
		if err != nil {
			// The redirect carries only the numeric code; the detail that
			// explains it (upstream status, mapping failure, ...) lives here.
			logger.Warn("oauth: callback failed",
				zap.String("request_id", c.GetString(middleware.RequestIDKey)),
				zap.String("slug", slug),
				zap.Error(err))
			c.Redirect(http.StatusFound, "/login?oauth_error="+strconv.Itoa(oauthErrorCode(err)))
			return
		}
		writeSessionCookie(c, sessionID, int(auth.SessionTTL.Seconds()))
		c.Redirect(http.StatusFound, "/")
	}
}

// oauthErrorCode maps callback failures onto their errcode numbers for the
// redirect query. Unknown errors collapse to the generic exchange failure
// — the login page shows a retryable message either way, and the server
// log carries the detail.
func oauthErrorCode(err error) int {
	switch {
	case errors.Is(err, errcode.ErrOAuthProviderNotFound):
		return errcode.OAuthProviderNotFound
	case errors.Is(err, errcode.ErrOAuthStateInvalid):
		return errcode.OAuthStateInvalid
	case errors.Is(err, errcode.ErrOAuthUserinfoFailed):
		return errcode.OAuthUserinfoFailed
	case errors.Is(err, errcode.ErrAccountDisabled):
		return errcode.AccountDisabled
	default:
		return errcode.OAuthExchangeFailed
	}
}

// === Admin provider management ============================================

type oauthProviderRequest struct {
	// min=3 matches alnum_dash's own {3,32} pattern — a declared min the
	// validator can never satisfy would just make short slugs 400 with a
	// misleading message.
	Slug                  string `json:"slug" binding:"required,min=3,max=32,alnum_dash"`
	Name                  string `json:"name" binding:"required,max=64"`
	Icon                  string `json:"icon" binding:"omitempty,max=255"`
	Enabled               bool   `json:"enabled"`
	ClientID              string `json:"client_id" binding:"required,max=255"`
	ClientSecret          string `json:"client_secret" binding:"required"`
	AuthorizationEndpoint string `json:"authorization_endpoint" binding:"required,url,max=512"`
	TokenEndpoint         string `json:"token_endpoint" binding:"required,url,max=512"`
	UserinfoEndpoint      string `json:"userinfo_endpoint" binding:"required,url,max=512"`
	// Scopes nil = OIDC default; an explicit empty is a valid choice
	// (Feishu takes no scope names) and must reach the service as nil-vs-
	// empty distinguishable.
	Scopes              *string `json:"scopes" binding:"omitempty,max=255"`
	UserIDField         string  `json:"user_id_field" binding:"omitempty,max=128"`
	UsernameField       string  `json:"username_field" binding:"omitempty,max=128"`
	DisplayNameField    string  `json:"display_name_field" binding:"omitempty,max=128"`
	EmailField          string  `json:"email_field" binding:"omitempty,max=128"`
	AuthStyle           string  `json:"auth_style" binding:"omitempty,oneof=basic post"`
	TokenRequestStyle   string  `json:"token_request_style" binding:"omitempty,oneof=form json"`
	TokenFieldStyle     string  `json:"token_field_style" binding:"omitempty,oneof=snake camel"`
	UserinfoTokenHeader string  `json:"userinfo_token_header" binding:"omitempty,max=128"`
	// PkceEnabled nil = default (on): a request that never mentions the
	// knob must not silently turn PKCE off.
	PkceEnabled          *bool  `json:"pkce_enabled"`
	ExtraAuthorizeParams string `json:"extra_authorize_params" binding:"omitempty,max=512"`
}

// GetOAuthProviders handles GET /api/admin/oauth-providers. Besides the
// provider list it returns callback_base — this deployment's callback
// address prefix (server.external_url when configured, else derived from
// the request) — so the admin form can show the exact redirect_uri to
// register at the IdP. The form must not derive it from its own page
// origin: an admin browsing over LAN while external_url points at the
// public HTTPS origin would otherwise copy a mismatched redirect_uri.
func GetOAuthProviders(svc *oauth.OAuthProviderService, externalURL string) gin.HandlerFunc {
	return func(c *gin.Context) {
		views, err := svc.ListProviders()
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		// callbackURL with an empty slug yields ".../oauth/callback/", the
		// prefix the form appends the live slug to.
		response.Success(c, gin.H{"providers": views, "callback_base": callbackURL(c, "", externalURL)})
	}
}

// PostOAuthProvider handles POST /api/admin/oauth-providers.
func PostOAuthProvider(svc *oauth.OAuthProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req oauthProviderRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.CreateProvider(oauth.CreateOAuthProviderInput{
			Slug: req.Slug, Name: req.Name, Icon: req.Icon, Enabled: req.Enabled,
			ClientID: req.ClientID, ClientSecret: req.ClientSecret,
			AuthorizationEndpoint: req.AuthorizationEndpoint,
			TokenEndpoint:         req.TokenEndpoint,
			UserinfoEndpoint:      req.UserinfoEndpoint,
			Scopes:                req.Scopes,
			UserIDField:           req.UserIDField,
			UsernameField:         req.UsernameField,
			DisplayNameField:      req.DisplayNameField,
			EmailField:            req.EmailField,
			AuthStyle:             req.AuthStyle,
			TokenRequestStyle:     req.TokenRequestStyle,
			TokenFieldStyle:       req.TokenFieldStyle,
			UserinfoTokenHeader:   req.UserinfoTokenHeader,
			PkceEnabled:           req.PkceEnabled,
			ExtraAuthorizeParams:  req.ExtraAuthorizeParams,
		}, time.Now().UTC())
		if errors.Is(err, errcode.ErrOAuthProviderSlugTaken) {
			middleware.WriteAdminError(c, http.StatusConflict, errcode.OAuthProviderSlugTaken)
			return
		}
		if errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
			middleware.WriteAdminError(c, http.StatusBadRequest, errcode.OAuthProviderConfigInvalid)
			return
		}
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		response.Success(c, view)
	}
}

type oauthProviderPatchRequest struct {
	Name                  *string `json:"name" binding:"omitempty,max=64"`
	Icon                  *string `json:"icon" binding:"omitempty,max=255"`
	Enabled               *bool   `json:"enabled"`
	ClientID              *string `json:"client_id" binding:"omitempty,max=255"`
	ClientSecret          *string `json:"client_secret"`
	AuthorizationEndpoint *string `json:"authorization_endpoint" binding:"omitempty,url,max=512"`
	TokenEndpoint         *string `json:"token_endpoint" binding:"omitempty,url,max=512"`
	UserinfoEndpoint      *string `json:"userinfo_endpoint" binding:"omitempty,url,max=512"`
	Scopes                *string `json:"scopes" binding:"omitempty,max=255"`
	UserIDField           *string `json:"user_id_field" binding:"omitempty,max=128"`
	UsernameField         *string `json:"username_field" binding:"omitempty,max=128"`
	DisplayNameField      *string `json:"display_name_field" binding:"omitempty,max=128"`
	EmailField            *string `json:"email_field" binding:"omitempty,max=128"`
	AuthStyle             *string `json:"auth_style" binding:"omitempty,oneof=basic post"`
	TokenRequestStyle     *string `json:"token_request_style" binding:"omitempty,oneof=form json"`
	TokenFieldStyle       *string `json:"token_field_style" binding:"omitempty,oneof=snake camel"`
	UserinfoTokenHeader   *string `json:"userinfo_token_header" binding:"omitempty,max=128"`
	PkceEnabled           *bool   `json:"pkce_enabled"`
	ExtraAuthorizeParams  *string `json:"extra_authorize_params" binding:"omitempty,max=512"`
}

// PatchOAuthProvider handles PATCH /api/admin/oauth-providers/:id.
func PatchOAuthProvider(svc *oauth.OAuthProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		var req oauthProviderPatchRequest
		if !bindJSON(c, &req) {
			return
		}
		view, err := svc.UpdateProvider(id, oauth.UpdateOAuthProviderInput{
			Name: req.Name, Icon: req.Icon, Enabled: req.Enabled,
			ClientID: req.ClientID, ClientSecret: req.ClientSecret,
			AuthorizationEndpoint: req.AuthorizationEndpoint,
			TokenEndpoint:         req.TokenEndpoint,
			UserinfoEndpoint:      req.UserinfoEndpoint,
			Scopes:                req.Scopes,
			UserIDField:           req.UserIDField,
			UsernameField:         req.UsernameField,
			DisplayNameField:      req.DisplayNameField,
			EmailField:            req.EmailField,
			AuthStyle:             req.AuthStyle,
			TokenRequestStyle:     req.TokenRequestStyle,
			TokenFieldStyle:       req.TokenFieldStyle,
			UserinfoTokenHeader:   req.UserinfoTokenHeader,
			PkceEnabled:           req.PkceEnabled,
			ExtraAuthorizeParams:  req.ExtraAuthorizeParams,
		}, time.Now().UTC())
		if errors.Is(err, errcode.ErrOAuthProviderNotFound) {
			middleware.WriteAdminError(c, http.StatusNotFound, errcode.OAuthProviderNotFound)
			return
		}
		if errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
			middleware.WriteAdminError(c, http.StatusBadRequest, errcode.OAuthProviderConfigInvalid)
			return
		}
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		response.Success(c, view)
	}
}

// DeleteOAuthProvider handles DELETE /api/admin/oauth-providers/:id.
func DeleteOAuthProvider(svc *oauth.OAuthProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		if err := svc.DeleteProvider(id); err != nil {
			if errors.Is(err, errcode.ErrOAuthProviderNotFound) {
				middleware.WriteAdminError(c, http.StatusNotFound, errcode.OAuthProviderNotFound)
				return
			}
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		response.Success(c, nil)
	}
}

type oauthDiscoverRequest struct {
	URL string `json:"url" binding:"required,max=512"`
}

// PostOAuthDiscover handles POST /api/admin/oauth-providers/discover —
// fetches an OIDC well-known document so the admin form can auto-fill the
// three endpoints.
func PostOAuthDiscover(svc *oauth.OAuthProviderService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req oauthDiscoverRequest
		if !bindJSON(c, &req) {
			return
		}
		result, err := svc.Discover(c.Request.Context(), req.URL)
		if err != nil {
			middleware.WriteAdminError(c, http.StatusBadGateway, errcode.OAuthDiscoveryFailed)
			return
		}
		response.Success(c, result)
	}
}
