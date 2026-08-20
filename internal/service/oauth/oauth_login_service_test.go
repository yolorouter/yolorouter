package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// fakeIdP is a minimal identity provider: a token endpoint that records
// what the exchange sent and a userinfo endpoint keyed by access token.
type fakeIdP struct {
	server *httptest.Server
	// captured by the token endpoint
	lastCode         string
	lastCodeVerifier string
	lastRedirectURI  string
	lastClientSecret string
	// configuration
	accessToken  string
	userinfo     map[string]any
	tokenAsForm  bool // answer form-encoded instead of JSON
	failExchange bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{accessToken: "at-123", userinfo: map[string]any{
		"sub": "u-1", "preferred_username": "Alice Wonder", "name": "Alice", "email": "alice@example.com",
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.lastCode = r.PostForm.Get("code")
		f.lastCodeVerifier = r.PostForm.Get("code_verifier")
		f.lastRedirectURI = r.PostForm.Get("redirect_uri")
		f.lastClientSecret = r.PostForm.Get("client_secret")
		if f.failExchange {
			http.Error(w, "denied", http.StatusBadRequest)
			return
		}
		if f.tokenAsForm {
			w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
			_, _ = w.Write([]byte("access_token=" + f.accessToken + "&token_type=bearer"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": f.accessToken})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.accessToken {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.userinfo)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// seedProviderForIdP stores a provider row pointing at the fake IdP.
func seedProviderForIdP(t *testing.T, db *gorm.DB, f *fakeIdP, slug string) *model.OAuthProvider {
	t.Helper()
	now := time.Now().UTC()
	encrypted, err := crypto.Encrypt(testutil.ProviderMasterKey(), "shh-secret")
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}
	p := &model.OAuthProvider{
		Slug: slug, Name: "Fake IdP", Enabled: true,
		ClientID: "client-1", EncryptedClientSecret: encrypted,
		AuthorizationEndpoint: f.server.URL + "/authorize",
		TokenEndpoint:         f.server.URL + "/token",
		UserinfoEndpoint:      f.server.URL + "/userinfo",
		Scopes:                "openid profile email",
		UserIDField:           "sub", UsernameField: "preferred_username",
		DisplayNameField: "name", EmailField: "email",
		AuthStyle: model.OAuthAuthStylePost,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repository.CreateOAuthProvider(db, p); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	return p
}

func newOAuthLoginServiceForTest(t *testing.T) (*OAuthLoginService, *gorm.DB) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	return NewOAuthLoginService(db, testutil.ProviderSecrets()), db
}

// beginAndExtractState runs BeginLogin and pulls the raw state token back
// out of the authorization URL — exactly what the provider would echo.
func beginAndExtractState(t *testing.T, svc *OAuthLoginService, slug string) (authorizeURL, state string) {
	t.Helper()
	authorizeURL, stateToken, err := svc.BeginLogin(slug, "http://app.local/oauth/callback/"+slug, time.Now().UTC())
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	if got := u.Query().Get("state"); got != stateToken {
		t.Fatalf("returned state token %q must equal the URL's state %q", stateToken, got)
	}
	return authorizeURL, stateToken
}

func TestOAuthLoginFullFlowAutoRegistersAndReusesAccount(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	seedProviderForIdP(t, db, idp, "fake")

	authorizeURL, state := beginAndExtractState(t, svc, "fake")
	u, _ := url.Parse(authorizeURL)
	q := u.Query()
	// The authorization request must carry the full PKCE + state shape and
	// the pinned redirect URI.
	if q.Get("response_type") != "code" || q.Get("client_id") != "client-1" {
		t.Fatalf("authorize url missing basics: %s", authorizeURL)
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize url missing PKCE: %s", authorizeURL)
	}
	if q.Get("redirect_uri") != "http://app.local/oauth/callback/fake" {
		t.Fatalf("unexpected redirect_uri: %q", q.Get("redirect_uri"))
	}

	sessionID, err := svc.HandleCallback(context.Background(), "fake", state, "code-abc", time.Now().UTC())
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if sessionID == "" {
		t.Fatalf("expected a session token")
	}
	// The exchange must have repeated the pinned redirect URI, sent the
	// PKCE verifier matching the challenge, and carried the decrypted
	// secret (post style).
	if idp.lastCode != "code-abc" || idp.lastRedirectURI != "http://app.local/oauth/callback/fake" {
		t.Fatalf("exchange sent wrong code/redirect: %q %q", idp.lastCode, idp.lastRedirectURI)
	}
	if idp.lastCodeVerifier == "" {
		t.Fatalf("exchange did not send a code_verifier")
	}
	if idp.lastClientSecret != "shh-secret" {
		t.Fatalf("exchange did not send the decrypted client secret, got %q", idp.lastClientSecret)
	}

	// The session resolves to a freshly registered enabled member with a
	// sanitized username and mapped profile fields.
	user, err := repository.FindUserByValidSession(db, sessionID, time.Now().UTC())
	if err != nil {
		t.Fatalf("session lookup: %v", err)
	}
	if user.Role != model.RoleMember || user.Status != model.UserStatusEnabled || user.IsLocal {
		t.Fatalf("unexpected auto-registered account: %+v", user)
	}
	if user.Username != "alice-wonder" {
		t.Fatalf("expected sanitized username alice-wonder, got %q", user.Username)
	}
	if user.Email != "alice@example.com" || user.DisplayName != "Alice" {
		t.Fatalf("profile fields not mapped: %+v", user)
	}

	// Second login with the same provider identity must reuse the account,
	// not create a sibling.
	_, state2 := beginAndExtractState(t, svc, "fake")
	session2, err := svc.HandleCallback(context.Background(), "fake", state2, "code-def", time.Now().UTC())
	if err != nil {
		t.Fatalf("second HandleCallback: %v", err)
	}
	user2, err := repository.FindUserByValidSession(db, session2, time.Now().UTC())
	if err != nil {
		t.Fatalf("second session lookup: %v", err)
	}
	if user2.ID != user.ID {
		t.Fatalf("second login created a new account: %d vs %d", user2.ID, user.ID)
	}
	var userCount int64
	if err := db.Model(&model.User{}).Count(&userCount).Error; err != nil {
		t.Fatalf("count users: %v", err)
	}
	if userCount != 1 {
		t.Fatalf("expected exactly 1 account, got %d", userCount)
	}
}

// TestOAuthCallbackRejectsReplayedState is the core anti-replay property:
// a state spends on first use, even though that first use succeeded.
func TestOAuthCallbackRejectsReplayedState(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	seedProviderForIdP(t, db, idp, "fake")

	_, state := beginAndExtractState(t, svc, "fake")
	if _, err := svc.HandleCallback(context.Background(), "fake", state, "code-1", time.Now().UTC()); err != nil {
		t.Fatalf("first callback: %v", err)
	}
	_, err := svc.HandleCallback(context.Background(), "fake", state, "code-2", time.Now().UTC())
	if !errors.Is(err, errcode.ErrOAuthStateInvalid) {
		t.Fatalf("expected ErrOAuthStateInvalid on replay, got %v", err)
	}
}

// TestOAuthStateDiesEvenWhenExchangeFails pins consume-before-exchange: a
// failed exchange must not leave the state alive for a second attempt.
func TestOAuthStateDiesEvenWhenExchangeFails(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	seedProviderForIdP(t, db, idp, "fake")
	idp.failExchange = true

	_, state := beginAndExtractState(t, svc, "fake")
	if _, err := svc.HandleCallback(context.Background(), "fake", state, "code-1", time.Now().UTC()); !errors.Is(err, errcode.ErrOAuthExchangeFailed) {
		t.Fatalf("expected exchange failure, got %v", err)
	}

	idp.failExchange = false
	_, err := svc.HandleCallback(context.Background(), "fake", state, "code-2", time.Now().UTC())
	if !errors.Is(err, errcode.ErrOAuthStateInvalid) {
		t.Fatalf("expected the state to be dead after the failed attempt, got %v", err)
	}
}

// TestOAuthStateBoundToItsProvider: a state issued for provider A must not
// authorize provider B's callback.
func TestOAuthStateBoundToItsProvider(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	seedProviderForIdP(t, db, idp, "prov-a")
	seedProviderForIdP(t, db, idp, "prov-b")

	_, state := beginAndExtractState(t, svc, "prov-a")
	_, err := svc.HandleCallback(context.Background(), "prov-b", state, "code-1", time.Now().UTC())
	if !errors.Is(err, errcode.ErrOAuthStateInvalid) {
		t.Fatalf("expected cross-provider state to be invalid, got %v", err)
	}
}

func TestOAuthCallbackRejectsExpiredState(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	seedProviderForIdP(t, db, idp, "fake")

	_, state := beginAndExtractState(t, svc, "fake")
	after := time.Now().UTC().Add(AuthStateTTL + time.Minute)
	_, err := svc.HandleCallback(context.Background(), "fake", state, "code-1", after)
	if !errors.Is(err, errcode.ErrOAuthStateInvalid) {
		t.Fatalf("expected expired state to be invalid, got %v", err)
	}
}

// TestOAuthDisabledProviderIsInvisible: begin AND callback both answer
// not-found once the provider is switched off.
func TestOAuthDisabledProviderIsInvisible(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	p := seedProviderForIdP(t, db, idp, "fake")

	_, state := beginAndExtractState(t, svc, "fake")
	if err := db.Model(&model.OAuthProvider{}).Where("id = ?", p.ID).Update("enabled", false).Error; err != nil {
		t.Fatalf("disable provider: %v", err)
	}

	if _, _, err := svc.BeginLogin("fake", "http://app.local/cb", time.Now().UTC()); !errors.Is(err, errcode.ErrOAuthProviderNotFound) {
		t.Fatalf("expected BeginLogin not-found for disabled provider, got %v", err)
	}
	if _, err := svc.HandleCallback(context.Background(), "fake", state, "c", time.Now().UTC()); !errors.Is(err, errcode.ErrOAuthProviderNotFound) {
		t.Fatalf("expected callback not-found for disabled provider, got %v", err)
	}
}

// TestOAuthCallbackRejectsDisabledUser: the account exists but is
// disabled — the callback must refuse a session even though the identity
// resolved.
func TestOAuthCallbackRejectsDisabledUser(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	seedProviderForIdP(t, db, idp, "fake")

	_, state := beginAndExtractState(t, svc, "fake")
	session1, err := svc.HandleCallback(context.Background(), "fake", state, "c1", time.Now().UTC())
	if err != nil {
		t.Fatalf("first callback: %v", err)
	}
	user, err := repository.FindUserByValidSession(db, session1, time.Now().UTC())
	if err != nil {
		t.Fatalf("session lookup: %v", err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", user.ID).Update("status", model.UserStatusDisabled).Error; err != nil {
		t.Fatalf("disable user: %v", err)
	}

	_, state2 := beginAndExtractState(t, svc, "fake")
	_, err = svc.HandleCallback(context.Background(), "fake", state2, "c2", time.Now().UTC())
	if !errors.Is(err, errcode.ErrAccountDisabled) {
		t.Fatalf("expected ErrAccountDisabled, got %v", err)
	}
}

// TestOAuthFormEncodedTokenResponse covers providers (GitHub) that answer
// the token exchange form-encoded rather than JSON.
func TestOAuthFormEncodedTokenResponse(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	idp.tokenAsForm = true
	seedProviderForIdP(t, db, idp, "fake")

	_, state := beginAndExtractState(t, svc, "fake")
	if _, err := svc.HandleCallback(context.Background(), "fake", state, "c1", time.Now().UTC()); err != nil {
		t.Fatalf("form-encoded exchange should succeed: %v", err)
	}
}

// TestOAuthUsernameCollisionGetsSuffix: two different identities with the
// same upstream username coexist as user, user-2.
func TestOAuthUsernameCollisionGetsSuffix(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	seedProviderForIdP(t, db, idp, "fake")

	_, state := beginAndExtractState(t, svc, "fake")
	if _, err := svc.HandleCallback(context.Background(), "fake", state, "c1", time.Now().UTC()); err != nil {
		t.Fatalf("first callback: %v", err)
	}

	idp.userinfo["sub"] = "u-2" // different identity, same username claim
	_, state2 := beginAndExtractState(t, svc, "fake")
	session2, err := svc.HandleCallback(context.Background(), "fake", state2, "c2", time.Now().UTC())
	if err != nil {
		t.Fatalf("second callback: %v", err)
	}
	user2, err := repository.FindUserByValidSession(db, session2, time.Now().UTC())
	if err != nil {
		t.Fatalf("session lookup: %v", err)
	}
	if user2.Username != "alice-wonder-2" {
		t.Fatalf("expected suffixed username alice-wonder-2, got %q", user2.Username)
	}
}

// TestOAuthNumericProviderUserID covers GitHub-shaped userinfo: numeric id,
// custom field names.
func TestOAuthNumericProviderUserID(t *testing.T) {
	svc, db := newOAuthLoginServiceForTest(t)
	idp := newFakeIdP(t)
	idp.userinfo = map[string]any{"id": float64(9042), "login": "octo", "name": "Octo Cat"}
	p := seedProviderForIdP(t, db, idp, "fake")
	if err := db.Model(&model.OAuthProvider{}).Where("id = ?", p.ID).Updates(map[string]any{
		"user_id_field": "id", "username_field": "login", "email_field": "email",
	}).Error; err != nil {
		t.Fatalf("update mapping: %v", err)
	}

	_, state := beginAndExtractState(t, svc, "fake")
	if _, err := svc.HandleCallback(context.Background(), "fake", state, "c1", time.Now().UTC()); err != nil {
		t.Fatalf("callback: %v", err)
	}
	ident, err := repository.FindIdentity(db, p.ID, "9042")
	if err != nil {
		t.Fatalf("expected identity stored under the numeric id as text: %v", err)
	}
	user, err := repository.FindUserByID(db, ident.UserID)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if user.Username != "octo" {
		t.Fatalf("expected username octo, got %q", user.Username)
	}
}

func TestJSONPathStringWalksNestedPaths(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"data":{"user":{"id":42,"tags":["x"]}},"ok":true}`), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := jsonPathString(doc, "data.user.id"); got != "42" {
		t.Fatalf("nested numeric: got %q", got)
	}
	if got := jsonPathString(doc, "ok"); got != "true" {
		t.Fatalf("bool: got %q", got)
	}
	if got := jsonPathString(doc, "data.user.tags"); got != "" {
		t.Fatalf("non-scalar should map to empty, got %q", got)
	}
	if got := jsonPathString(doc, "missing.path"); got != "" {
		t.Fatalf("missing path should map to empty, got %q", got)
	}
}

func TestSanitizeUsernameFoldsToLocalCharset(t *testing.T) {
	cases := map[string]string{
		"Alice Wonder":          "alice-wonder",
		"  weird__Name ":        "weird--name",
		"名无":                    "",
		"---":                   "",
		strings.Repeat("a", 40): strings.Repeat("a", 32),
	}
	for in, want := range cases {
		if got := sanitizeUsername(in); got != want {
			t.Fatalf("sanitizeUsername(%q) = %q, want %q", in, got, want)
		}
	}
}
