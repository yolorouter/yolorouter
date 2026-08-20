package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/oauth"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// oauthTestMasterKey mirrors apiKeyTestMasterKey's shape for this file.
func oauthTestMasterKey() []byte { return []byte("0123456789abcdef0123456789abcdef") }

// newOAuthTestStack wires a fake IdP, a provider row pointing at it, and a
// router carrying the three public OAuth routes.
func newOAuthTestStack(t *testing.T) (*gin.Engine, *gorm.DB, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators: %v", err)
	}
	db := testutil.NewSQLiteDB(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-1"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "u-1", "preferred_username": "alice"})
	})
	idp := httptest.NewServer(mux)
	t.Cleanup(idp.Close)

	encrypted, err := crypto.Encrypt(oauthTestMasterKey(), "s3cret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	now := time.Now().UTC()
	if err := repository.CreateOAuthProvider(db, &model.OAuthProvider{
		Slug: "fake", Name: "Fake", Enabled: true,
		ClientID: "cid", EncryptedClientSecret: encrypted,
		AuthorizationEndpoint: idp.URL + "/authorize",
		TokenEndpoint:         idp.URL + "/token",
		UserinfoEndpoint:      idp.URL + "/userinfo",
		Scopes:                "openid", UserIDField: "sub", UsernameField: "preferred_username",
		DisplayNameField: "name", EmailField: "email", AuthStyle: model.OAuthAuthStylePost,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	svc := oauth.NewOAuthLoginService(db, crypto.NewSecretBox(oauthTestMasterKey()))
	r := gin.New()
	r.GET("/api/admin/auth/oauth/providers", GetPublicOAuthProviders(svc))
	r.POST("/api/admin/auth/oauth/state", PostOAuthState(svc, middleware.NewSemaphore(8),
		middleware.NewRateWindow(1000, time.Minute), middleware.NewPerClientRateWindow(1000, time.Minute), ""))
	r.GET("/oauth/callback/:slug", GetOAuthCallback(svc, middleware.NewSemaphore(8)))
	return r, db, idp
}

// beginViaHTTP drives POST /auth/oauth/state and returns the state token
// parsed back out of the authorization URL, plus the browser-binding
// state cookie the endpoint set (the callback requires it).
func beginViaHTTP(t *testing.T, r *gin.Engine) (state string, stateCookie *http.Cookie) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/oauth/state",
		strings.NewReader(`{"slug":"fake"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "app.local"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("state endpoint: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			AuthorizeURL string `json:"authorize_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	u, err := url.Parse(env.Data.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	// The redirect URI must be derived from the request's own host.
	if got := u.Query().Get("redirect_uri"); got != "http://app.local/oauth/callback/fake" {
		t.Fatalf("unexpected derived redirect_uri: %q", got)
	}
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "oauth_state" {
			stateCookie = ck
		}
	}
	if stateCookie == nil || stateCookie.Value == "" {
		t.Fatalf("state endpoint did not set the oauth_state cookie")
	}
	return u.Query().Get("state"), stateCookie
}

func TestOAuthCallbackSetsSessionCookieAndRedirectsHome(t *testing.T) {
	r, db, _ := newOAuthTestStack(t)
	state, stateCookie := beginViaHTTP(t, r)

	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/fake?state="+url.QueryEscape(state)+"&code=c1", nil)
	req.AddCookie(stateCookie)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("expected redirect to /, got %q", loc)
	}
	var sessionCookie *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "session_id" {
			sessionCookie = ck
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatalf("expected a session_id cookie, got %+v", w.Result().Cookies())
	}
	user, err := repository.FindUserByValidSession(db, sessionCookie.Value, time.Now().UTC())
	if err != nil {
		t.Fatalf("cookie does not resolve to a session: %v", err)
	}
	if user.Role != model.RoleMember {
		t.Fatalf("expected an auto-registered member, got %+v", user)
	}
}

func TestOAuthCallbackFailureRedirectsToLoginWithErrorCode(t *testing.T) {
	r, _, _ := newOAuthTestStack(t)

	// A bogus state with a "matching" cookie passes the browser-binding
	// check and must then die on the server-side state lookup.
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/fake?state=bogus&code=c1", nil)
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: crypto.HashToken("bogus")})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	wantLoc := "/login?oauth_error=" + strconv.Itoa(errcode.OAuthStateInvalid)
	if loc := w.Header().Get("Location"); loc != wantLoc {
		t.Fatalf("expected redirect %q, got %q", wantLoc, loc)
	}
	// A failed callback must not plant a session cookie.
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "session_id" && ck.Value != "" {
			t.Fatalf("failed callback planted a session cookie")
		}
	}
}

// TestOAuthCallbackRequiresInitiatingBrowserCookie is the login-CSRF
// defense: a perfectly valid state completed in a browser that never
// started the flow (no oauth_state cookie, or a mismatched one) must be
// rejected WITHOUT consuming the state — an attacker-delivered
// authorization URL dies in the victim's browser. Remove the cookie
// check in GetOAuthCallback and this goes red.
func TestOAuthCallbackRequiresInitiatingBrowserCookie(t *testing.T) {
	r, db, _ := newOAuthTestStack(t)
	state, stateCookie := beginViaHTTP(t, r)

	// No cookie at all.
	req := httptest.NewRequest(http.MethodGet, "/oauth/callback/fake?state="+url.QueryEscape(state)+"&code=c1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	wantLoc := "/login?oauth_error=" + strconv.Itoa(errcode.OAuthStateInvalid)
	if w.Code != http.StatusFound || w.Header().Get("Location") != wantLoc {
		t.Fatalf("expected cookie-less callback to be rejected, got %d %q", w.Code, w.Header().Get("Location"))
	}

	// Mismatched cookie (a different browser's pending flow).
	req = httptest.NewRequest(http.MethodGet, "/oauth/callback/fake?state="+url.QueryEscape(state)+"&code=c1", nil)
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: crypto.HashToken("some-other-state")})
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != wantLoc {
		t.Fatalf("expected mismatched-cookie callback to be rejected, got %d %q", w.Code, w.Header().Get("Location"))
	}

	// The state itself must have survived both rejections — the legitimate
	// browser (holding the real cookie) can still complete the flow.
	req = httptest.NewRequest(http.MethodGet, "/oauth/callback/fake?state="+url.QueryEscape(state)+"&code=c1", nil)
	req.AddCookie(stateCookie)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/" {
		t.Fatalf("legitimate browser should still complete the flow, got %d %q", w.Code, w.Header().Get("Location"))
	}
	if _, err := repository.FindUserByValidSession(db, sessionCookieValue(t, w), time.Now().UTC()); err != nil {
		t.Fatalf("expected a working session after the legitimate completion: %v", err)
	}
}

// sessionCookieValue extracts the session_id cookie a successful callback
// set.
func sessionCookieValue(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "session_id" && ck.Value != "" {
			return ck.Value
		}
	}
	t.Fatalf("no session_id cookie in response")
	return ""
}

func TestPublicOAuthProviderListShape(t *testing.T) {
	r, _, _ := newOAuthTestStack(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/oauth/providers", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"slug":"fake"`) {
		t.Fatalf("expected the enabled provider in the list: %s", body)
	}
	for _, forbidden := range []string{"client_id", "token_endpoint", "client_secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public list leaks %q: %s", forbidden, body)
		}
	}
}

// TestCallbackURLPrefersConfiguredExternalURL pins the trusted-origin
// path: with server.external_url set, the redirect URI ignores the
// client-controlled Host header entirely; without it, derivation from
// Host/X-Forwarded-Proto remains the zero-config fallback.
func TestCallbackURLPrefersConfiguredExternalURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/auth/oauth/state", nil)
	c.Request.Host = "evil.example"
	c.Request.Header.Set("X-Forwarded-Proto", "https")

	if got := callbackURL(c, "github", "https://router.example.com/"); got != "https://router.example.com/oauth/callback/github" {
		t.Fatalf("configured external URL must win over Host, got %q", got)
	}
	if got := callbackURL(c, "github", ""); got != "https://evil.example/oauth/callback/github" {
		t.Fatalf("unexpected derived fallback: %q", got)
	}
}

// TestAdminProviderListCarriesCallbackBase: the admin list response must
// carry callback_base so the form shows the redirect_uri this deployment
// actually uses — the configured external_url verbatim when set, else the
// request-derived origin. Registering a page-origin-derived URL from a LAN
// session against a public external_url would break every IdP login.
func TestAdminProviderListCarriesCallbackBase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	svc := oauth.NewOAuthProviderService(db, crypto.NewSecretBox(oauthTestMasterKey()))

	fetchBase := func(externalURL string) string {
		r := gin.New()
		r.GET("/api/admin/oauth-providers", GetOAuthProviders(svc, externalURL))
		req := httptest.NewRequest(http.MethodGet, "/api/admin/oauth-providers", nil)
		req.Host = "lan.local:8084"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("list providers: %d %s", w.Code, w.Body.String())
		}
		var env struct {
			Data struct {
				CallbackBase string `json:"callback_base"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return env.Data.CallbackBase
	}

	if got := fetchBase("https://public.example.com/"); got != "https://public.example.com/oauth/callback/" {
		t.Fatalf("configured external_url: unexpected callback_base %q", got)
	}
	if got := fetchBase(""); got != "http://lan.local:8084/oauth/callback/" {
		t.Fatalf("derived origin: unexpected callback_base %q", got)
	}
}
