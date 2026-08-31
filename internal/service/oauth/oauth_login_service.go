package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/auth"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// AuthStateTTL bounds how long a login attempt may sit between "clicked
// the provider button" and "came back through the callback".
const AuthStateTTL = 10 * time.Minute

// OAuthLoginService drives the authorization-code + PKCE login flow
// against any configured provider.
type OAuthLoginService struct {
	db      *gorm.DB
	secrets crypto.SecretBox
	// httpClient talks to the provider's token and userinfo endpoints.
	// These URLs are admin-supplied configuration (see
	// OAuthProviderService.httpClient for the trust rationale).
	httpClient *http.Client
}

func NewOAuthLoginService(db *gorm.DB, secrets crypto.SecretBox) *OAuthLoginService {
	return &OAuthLoginService{db: db, secrets: secrets, httpClient: &http.Client{Timeout: 15 * time.Second}}
}

// PublicProviderView is what the login page sees: enough to render a
// button, nothing else — no client_id, no endpoints.
type PublicProviderView struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

// ListEnabledProviders returns the login page's button list.
func (s *OAuthLoginService) ListEnabledProviders() ([]PublicProviderView, error) {
	rows, err := repository.ListEnabledOAuthProviders(s.db)
	if err != nil {
		return nil, err
	}
	views := make([]PublicProviderView, 0, len(rows))
	for _, p := range rows {
		views = append(views, PublicProviderView{Slug: p.Slug, Name: p.Name, Icon: p.Icon})
	}
	return views, nil
}

// BeginLogin issues a one-time state + PKCE pair for the provider and
// returns the full authorization URL for the browser to navigate to,
// along with the raw state token (the handler binds it to the initiating
// browser via a cookie — see GetOAuthCallback). The URL is assembled
// server-side so client_id and endpoints never appear in any public API
// response, and redirectURI is pinned into the state row so the token
// exchange later repeats the exact value.
func (s *OAuthLoginService) BeginLogin(slug, redirectURI string, now time.Time) (authorizeURL, stateToken string, err error) {
	provider, err := s.findEnabledProvider(slug)
	if err != nil {
		return "", "", err
	}

	// Sweep expired states opportunistically — the anonymous state
	// endpoint inserts a row per call, and abandoned attempts would
	// otherwise only be cleaned on successful consumes.
	if err := repository.DeleteExpiredAuthStates(s.db, now); err != nil {
		return "", "", err
	}

	stateToken, err = crypto.GenerateRandomToken(32, "")
	if err != nil {
		return "", "", err
	}
	codeVerifier, err := crypto.GenerateRandomToken(32, "")
	if err != nil {
		return "", "", err
	}
	if err := repository.CreateAuthState(s.db, stateToken, provider.ID, codeVerifier, redirectURI,
		now.Add(AuthStateTTL), now); err != nil {
		return "", "", err
	}

	challenge := sha256.Sum256([]byte(codeVerifier))
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", provider.ClientID)
	q.Set("redirect_uri", redirectURI)
	// An omitted scope is valid OAuth2; an empty `scope=` is not parsed as
	// one by every IdP (Feishu presets an empty scope list).
	if scopes := strings.TrimSpace(provider.Scopes); scopes != "" {
		q.Set("scope", scopes)
	}
	q.Set("state", stateToken)
	// PKCE-less providers (DingTalk's authorize endpoint has no
	// code_challenge parameter) still mint and store a verifier in the
	// state row — it is simply never presented.
	if provider.PkceEnabled {
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
		q.Set("code_challenge_method", "S256")
	}
	// Provider-specific authorize parameters (DingTalk: prompt=consent).
	// Write-time validation already rejects reserved keys; the skip here is
	// defense for rows that reached the database through another path.
	if extra, err := url.ParseQuery(provider.ExtraAuthorizeParams); err == nil {
		for k, vs := range extra {
			if reservedAuthorizeParams[k] {
				continue
			}
			for _, v := range vs {
				q.Add(k, v)
			}
		}
	}

	sep := "?"
	if strings.Contains(provider.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return provider.AuthorizationEndpoint + sep + q.Encode(), stateToken, nil
}

// HandleCallback completes the flow: consume the state, exchange the code,
// read the user identity, find or auto-register the account, and issue a
// session. Returns the raw session token for the cookie.
func (s *OAuthLoginService) HandleCallback(ctx context.Context, slug, state, code string, now time.Time) (string, error) {
	provider, err := s.findEnabledProvider(slug)
	if err != nil {
		return "", err
	}
	if state == "" || code == "" {
		return "", errcode.ErrOAuthStateInvalid
	}
	// Consume BEFORE the outbound exchange: a state must die on first use
	// even when the exchange after it fails, or a failed callback could be
	// replayed with a different code.
	stateRow, err := repository.ConsumeAuthState(s.db, state, provider.ID, now)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", errcode.ErrOAuthStateInvalid
	}
	if err != nil {
		return "", err
	}

	accessToken, err := s.exchangeCode(ctx, provider, code, stateRow.CodeVerifier, stateRow.RedirectURI)
	if err != nil {
		return "", err
	}
	identity, err := s.fetchUserinfo(ctx, provider, accessToken)
	if err != nil {
		return "", err
	}

	user, err := s.findOrCreateUser(provider, identity, now)
	if err != nil {
		return "", err
	}
	if user.Status != model.UserStatusEnabled {
		return "", errcode.ErrAccountDisabled
	}

	// Same shape as password login: sweep expired sessions, stamp
	// last_login_at, and issue the session in one transaction.
	var sessionID string
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := repository.DeleteExpiredSessions(tx, now); err != nil {
			return err
		}
		if err := repository.RecordLoginSuccess(tx, user.ID, now); err != nil {
			return err
		}
		var sessErr error
		sessionID, sessErr = auth.CreateSession(tx, user.ID, now)
		return sessErr
	}); err != nil {
		return "", err
	}
	return sessionID, nil
}

func (s *OAuthLoginService) findEnabledProvider(slug string) (*model.OAuthProvider, error) {
	provider, err := repository.FindOAuthProviderBySlug(s.db, slug)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errcode.ErrOAuthProviderNotFound
	}
	if err != nil {
		return nil, err
	}
	// A disabled provider answers exactly like a missing one — the login
	// page doesn't offer it, so anyone reaching it is probing.
	if !provider.Enabled {
		return nil, errcode.ErrOAuthProviderNotFound
	}
	return provider, nil
}

// exchangeCode swaps the authorization code for an access token. The
// standard path posts form-encoded and accepts the answer as JSON or
// form-encoded (GitHub answers the latter unless asked otherwise; Accept:
// application/json asks, the fallback covers providers that ignore it).
// Providers whose token endpoint speaks JSON instead (Feishu, DingTalk)
// take the exchangeCodeJSON path via token_request_style.
func (s *OAuthLoginService) exchangeCode(ctx context.Context, p *model.OAuthProvider, code, codeVerifier, redirectURI string) (string, error) {
	secret, err := s.secrets.Decrypt(p.EncryptedClientSecret)
	if err != nil {
		return "", fmt.Errorf("%w: decrypt client secret: %v", errcode.ErrOAuthExchangeFailed, err)
	}
	// A PKCE-less provider never presents its stored verifier, on either
	// request path.
	if !p.PkceEnabled {
		codeVerifier = ""
	}

	if p.TokenRequestStyle == model.OAuthTokenRequestStyleJSON {
		return s.exchangeCodeJSON(ctx, p, secret, code, codeVerifier, redirectURI)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	form.Set("client_id", p.ClientID)
	if p.AuthStyle != model.OAuthAuthStyleBasic {
		form.Set("client_secret", secret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: %v", errcode.ErrOAuthExchangeFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.AuthStyle == model.OAuthAuthStyleBasic {
		req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(secret))
	}

	body, err := s.doExchange(ctx, req)
	if err != nil {
		return "", err
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.AccessToken != "" {
		return parsed.AccessToken, nil
	}
	if vals, err := url.ParseQuery(string(body)); err == nil {
		if tok := vals.Get("access_token"); tok != "" {
			return tok, nil
		}
	}
	return "", fmt.Errorf("%w: no access_token in response", errcode.ErrOAuthExchangeFailed)
}

// doExchange performs a prepared token request and returns the raw response
// body, mapping transport errors and non-200 statuses onto the exchange
// failure error. IdPs report failures as an error document (GitHub text,
// Feishu errcode, DingTalk code/message) — an excerpt names the actual
// cause instead of a bare status.
func (s *OAuthLoginService) doExchange(ctx context.Context, req *http.Request) ([]byte, error) {
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrOAuthExchangeFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrOAuthExchangeFailed, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d: %s", errcode.ErrOAuthExchangeFailed, resp.StatusCode, responseExcerpt(body))
	}
	return body, nil
}

// exchangeCodeJSON is the JSON-body exchange for providers whose token
// endpoints take JSON (Feishu, DingTalk). Field names follow the provider's
// token_field_style: snake sends the standard set (grant_type, client_id,
// client_secret, code, redirect_uri, code_verifier), camel sends DingTalk's
// trimmed set (clientId, clientSecret, code, grantType — no redirect_uri or
// code_verifier). Client credentials only ever ride the body:
// auth_style has no meaning in JSON mode.
func (s *OAuthLoginService) exchangeCodeJSON(ctx context.Context, p *model.OAuthProvider, secret, code, codeVerifier, redirectURI string) (string, error) {
	var payload map[string]any
	if p.TokenFieldStyle == model.OAuthTokenFieldStyleCamel {
		payload = map[string]any{
			"clientId":     p.ClientID,
			"clientSecret": secret,
			"code":         code,
			"grantType":    "authorization_code",
		}
	} else {
		payload = map[string]any{
			"grant_type":    "authorization_code",
			"client_id":     p.ClientID,
			"client_secret": secret,
			"code":          code,
			"redirect_uri":  redirectURI,
		}
		if codeVerifier != "" {
			payload["code_verifier"] = codeVerifier
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errcode.ErrOAuthExchangeFailed, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenEndpoint, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("%w: %v", errcode.ErrOAuthExchangeFailed, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	body, err := s.doExchange(ctx, req)
	if err != nil {
		return "", err
	}

	var parsed struct {
		AccessToken      string `json:"access_token"` // snake style (Feishu)
		AccessTokenCamel string `json:"accessToken"`  // camel style (DingTalk)
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("%w: %v", errcode.ErrOAuthExchangeFailed, err)
	}
	token := parsed.AccessToken
	if p.TokenFieldStyle == model.OAuthTokenFieldStyleCamel {
		token = parsed.AccessTokenCamel
	}
	if token == "" {
		return "", fmt.Errorf("%w: no access token in response: %s", errcode.ErrOAuthExchangeFailed, responseExcerpt(body))
	}
	return token, nil
}

// responseExcerpt bounds an error-response body for an error string —
// enough of the IdP's own error document to name the cause, never the
// whole payload.
func responseExcerpt(body []byte) string {
	s := strings.TrimSpace(string(body))
	const max = 200
	if len([]rune(s)) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

// oauthIdentity is the mapped userinfo the rest of the flow consumes.
type oauthIdentity struct {
	ProviderUserID string
	Username       string
	DisplayName    string
	Email          string
}

func (s *OAuthLoginService) fetchUserinfo(ctx context.Context, p *model.OAuthProvider, accessToken string) (*oauthIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserinfoEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrOAuthUserinfoFailed, err)
	}
	// Most IdPs take the standard Bearer header; DingTalk takes its own
	// x-acs-dingtalk-access-token header instead.
	if p.UserinfoTokenHeader != "" {
		req.Header.Set(p.UserinfoTokenHeader, accessToken)
	} else {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrOAuthUserinfoFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrOAuthUserinfoFailed, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d: %s", errcode.ErrOAuthUserinfoFailed, resp.StatusCode, responseExcerpt(body))
	}

	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrOAuthUserinfoFailed, err)
	}
	id := &oauthIdentity{
		ProviderUserID: jsonPathString(doc, p.UserIDField),
		Username:       jsonPathString(doc, p.UsernameField),
		DisplayName:    jsonPathString(doc, p.DisplayNameField),
		Email:          jsonPathString(doc, p.EmailField),
	}
	// The provider user id is the identity — without it nothing else
	// matters. Everything else is optional display data.
	if id.ProviderUserID == "" {
		return nil, fmt.Errorf("%w: empty user id field %q", errcode.ErrOAuthUserinfoFailed, p.UserIDField)
	}
	return id, nil
}

// jsonPathString walks a dot-separated path through decoded JSON and
// renders the leaf as a string. Numbers format without an exponent so a
// numeric user id (GitHub) is stable text; other types map to "".
func jsonPathString(doc any, path string) string {
	if path == "" {
		return ""
	}
	cur := doc
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = m[part]
		if !ok {
			return ""
		}
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		// JSON numbers decode as float64; ids are integers in practice.
		// Integers beyond 2^53 would lose precision here — every known
		// provider (GitHub, GitLab, ...) issues far smaller numeric ids,
		// and OIDC's sub claim is a string, so this stays a documented
		// bound rather than a decimal re-parse.
		return fmt.Sprintf("%.0f", v)
	case bool:
		return fmt.Sprintf("%t", v)
	default:
		return ""
	}
}

// findOrCreateUser resolves the provider identity to an account,
// auto-registering a member on first login (per the access policy:
// passing the company IdP is the admission check). Creation of the user
// and the identity link happens in one transaction; a concurrent first
// login for the same identity loses on the identities unique index and
// re-reads the winner.
func (s *OAuthLoginService) findOrCreateUser(p *model.OAuthProvider, id *oauthIdentity, now time.Time) (*model.User, error) {
	ident, err := repository.FindIdentity(s.db, p.ID, id.ProviderUserID)
	if err == nil {
		return repository.FindUserByID(s.db, ident.UserID)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	var user *model.User
	txErr := s.db.Transaction(func(tx *gorm.DB) error {
		username, err := s.pickUsername(tx, id, p.Slug)
		if err != nil {
			return err
		}
		user = &model.User{
			Username: username,
			// Optional display claims are bounded to their column widths:
			// SQLite ignores declared VARCHAR lengths but PostgreSQL
			// enforces them, and an IdP-supplied 200-char display name must
			// not make first login fail on one backend only.
			DisplayName: truncateRunes(id.DisplayName, 128),
			Email:       truncateRunes(id.Email, 255),
			Role:        model.RoleMember,
			Status:      model.UserStatusEnabled,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := repository.CreateUser(tx, user); err != nil {
			return err
		}
		return repository.CreateIdentity(tx, &model.UserIdentity{
			UserID:          user.ID,
			OAuthProviderID: p.ID,
			ProviderUserID:  id.ProviderUserID,
			CreatedAt:       now,
		})
	})
	if txErr != nil {
		// A concurrent callback for the same brand-new identity may have won
		// the unique index race — if the identity now exists, use it.
		if ident, retryErr := repository.FindIdentity(s.db, p.ID, id.ProviderUserID); retryErr == nil {
			return repository.FindUserByID(s.db, ident.UserID)
		}
		return nil, txErr
	}
	return user, nil
}

// pickUsername derives a unique local username from the provider's
// username claim: sanitized to the local charset, then suffixed on
// collision. The provider username is display sugar — identity is the
// provider user id — so a mangled or suffixed result costs nothing.
func (s *OAuthLoginService) pickUsername(tx *gorm.DB, id *oauthIdentity, slug string) (string, error) {
	base := sanitizeUsername(id.Username)
	if base == "" {
		base = sanitizeUsername(slug + "-user")
	}
	candidate := base
	for i := 2; i <= 20; i++ {
		var count int64
		if err := tx.Model(&model.User{}).Where("username = ?", candidate).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return candidate, nil
		}
		candidate = truncateForSuffix(base, fmt.Sprintf("-%d", i))
	}
	// Pathological collision run — fall back to a random tail, which the
	// unique index makes effectively conflict-free.
	tail, err := crypto.GenerateRandomToken(4, "")
	if err != nil {
		return "", err
	}
	return truncateForSuffix(base, "-"+strings.ToLower(tail)), nil
}

// usernameMaxLen mirrors the setup form's upper bound so OAuth-provisioned
// usernames obey the same limit.
const usernameMaxLen = 32

// sanitizeUsername folds a provider username into the local charset
// (lowercase alphanumerics and dashes, no leading/trailing dash).
func sanitizeUsername(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > usernameMaxLen {
		out = out[:usernameMaxLen]
		out = strings.Trim(out, "-")
	}
	return out
}

// truncateRunes bounds s to at most n runes — rune-based so a multi-byte
// character is never split in half.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// truncateForSuffix appends suffix, shrinking the base first so the result
// stays within usernameMaxLen.
func truncateForSuffix(base, suffix string) string {
	maxBase := usernameMaxLen - len(suffix)
	if len(base) > maxBase {
		base = base[:maxBase]
		base = strings.Trim(base, "-")
	}
	return base + suffix
}
