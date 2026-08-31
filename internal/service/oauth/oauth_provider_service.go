package oauth

import (
	"context"
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
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// OAuthProviderService manages the admin-configured identity sources.
// Client secrets are AES-GCM encrypted under the same master key as
// upstream provider keys and never leave the server once written.
type OAuthProviderService struct {
	db      *gorm.DB
	secrets crypto.SecretBox
	// httpClient performs the OIDC discovery fetch. Provider endpoints are
	// admin-supplied configuration behind RequireAdmin — a self-hosted IdP
	// on a private address is the primary use case, so no private-address
	// blocking here (unlike gateway upstreams, these URLs are never
	// influenced by API callers).
	httpClient *http.Client
}

func NewOAuthProviderService(db *gorm.DB, secrets crypto.SecretBox) *OAuthProviderService {
	return &OAuthProviderService{
		db:         db,
		secrets:    secrets,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// OAuthProviderView is the admin-facing shape. The client secret is
// write-only: the view only reports whether one is stored.
type OAuthProviderView struct {
	ID                    uint   `json:"id"`
	Slug                  string `json:"slug"`
	Name                  string `json:"name"`
	Icon                  string `json:"icon"`
	Enabled               bool   `json:"enabled"`
	ClientID              string `json:"client_id"`
	HasClientSecret       bool   `json:"has_client_secret"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	Scopes                string `json:"scopes"`
	UserIDField           string `json:"user_id_field"`
	UsernameField         string `json:"username_field"`
	DisplayNameField      string `json:"display_name_field"`
	EmailField            string `json:"email_field"`
	AuthStyle             string `json:"auth_style"`
	// The protocol-style knobs reuse the model's declaration so the two
	// shapes cannot drift; embedding keeps the JSON keys flat.
	model.OAuthProtocolStyle
	IdentityCount int64     `json:"identity_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toOAuthProviderView(p model.OAuthProvider, identityCount int64) OAuthProviderView {
	return OAuthProviderView{
		ID: p.ID, Slug: p.Slug, Name: p.Name, Icon: p.Icon, Enabled: p.Enabled,
		ClientID: p.ClientID, HasClientSecret: p.EncryptedClientSecret != "",
		AuthorizationEndpoint: p.AuthorizationEndpoint, TokenEndpoint: p.TokenEndpoint,
		UserinfoEndpoint: p.UserinfoEndpoint, Scopes: p.Scopes,
		UserIDField: p.UserIDField, UsernameField: p.UsernameField,
		DisplayNameField: p.DisplayNameField, EmailField: p.EmailField,
		AuthStyle: p.AuthStyle, IdentityCount: identityCount,
		OAuthProtocolStyle: p.OAuthProtocolStyle,
		CreatedAt:          p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// CreateOAuthProviderInput carries every configurable field. Empty mapping
// fields fall back to the OIDC defaults at normalization.
type CreateOAuthProviderInput struct {
	Slug                  string
	Name                  string
	Icon                  string
	Enabled               bool
	ClientID              string
	ClientSecret          string
	AuthorizationEndpoint string
	TokenEndpoint         string
	UserinfoEndpoint      string
	// Scopes is fully admin-controlled: nil takes the OIDC default, an
	// explicit value (including empty — Feishu's authorize endpoint takes
	// no scope names and rejects OIDC-style ones) stores as-is.
	Scopes              *string
	UserIDField         string
	UsernameField       string
	DisplayNameField    string
	EmailField          string
	AuthStyle           string
	TokenRequestStyle   string
	TokenFieldStyle     string
	UserinfoTokenHeader string
	// PkceEnabled nil = the default (on): a create request that never
	// mentions the knob must not silently turn PKCE off.
	PkceEnabled          *bool
	ExtraAuthorizeParams string
}

// fallback returns v, or def when v is blank — the field-mapping and
// scope inputs all default to the standard OIDC claims.
func fallback(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

// fallbackScopes keeps the OIDC default for requests that never mentioned
// scopes, while letting an explicit empty through: some IdPs (Feishu) take
// no scope names at all and reject OIDC-style ones.
func fallbackScopes(v *string) string {
	if v == nil {
		return "openid profile email"
	}
	return strings.TrimSpace(*v)
}

func normalizeAuthStyle(v string) string {
	if v == model.OAuthAuthStyleBasic {
		return model.OAuthAuthStyleBasic
	}
	return model.OAuthAuthStylePost
}

// The protocol-style knobs normalize anything unrecognized to the standard
// shape — a provider configured before these columns existed (or through a
// path that skips the API) must keep behaving exactly as before.
func normalizeTokenRequestStyle(v string) string {
	if v == model.OAuthTokenRequestStyleJSON {
		return model.OAuthTokenRequestStyleJSON
	}
	return model.OAuthTokenRequestStyleForm
}

func normalizeTokenFieldStyle(v string) string {
	if v == model.OAuthTokenFieldStyleCamel {
		return model.OAuthTokenFieldStyleCamel
	}
	return model.OAuthTokenFieldStyleSnake
}

// reservedAuthorizeParams are assembled by BeginLogin itself, from state
// the login flow owns; extra authorize parameters colliding with them are
// rejected at write time and skipped defensively at assemble time.
var reservedAuthorizeParams = map[string]bool{
	"response_type": true, "client_id": true, "redirect_uri": true, "scope": true,
	"state": true, "code_challenge": true, "code_challenge_method": true,
}

// validateAuthorizeParams requires a URL-query-shaped string whose keys
// stay clear of the reserved set.
func validateAuthorizeParams(raw string) error {
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return errcode.ErrOAuthProviderConfigInvalid
	}
	for k := range vals {
		if reservedAuthorizeParams[k] {
			return errcode.ErrOAuthProviderConfigInvalid
		}
	}
	return nil
}

// validateProtocolKnobs checks the two free-form protocol knobs every
// write path shares: a custom userinfo header must be an RFC 7230 token,
// and extra authorize params a URL query clear of the reserved keys.
// Empty values pass — they mean the standard behavior.
func validateProtocolKnobs(header, params string) error {
	if h := strings.TrimSpace(header); h != "" {
		if err := validateHeaderName(h); err != nil {
			return err
		}
	}
	if p := strings.TrimSpace(params); p != "" {
		if err := validateAuthorizeParams(p); err != nil {
			return err
		}
	}
	return nil
}

// tchars are the RFC 7230 token characters valid in a header name.
const tchars = "!#$%&'*+-.^_`|~"

// validateHeaderName requires a non-empty RFC 7230 token of at most 128
// characters — the value becomes the header name on the userinfo request.
func validateHeaderName(name string) error {
	if name == "" || len(name) > 128 {
		return errcode.ErrOAuthProviderConfigInvalid
	}
	for _, r := range name {
		if r >= 0x80 {
			return errcode.ErrOAuthProviderConfigInvalid
		}
		if !strings.ContainsRune(tchars, r) &&
			(r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return errcode.ErrOAuthProviderConfigInvalid
		}
	}
	return nil
}

// validateEndpointURL requires an absolute http(s) URL with a host. Gin's
// `url` binding tag accepts any scheme — including javascript: — and the
// authorization endpoint in particular flows into a browser navigation on
// the login page, so scheme enforcement lives here where every write path
// (create, update, and discovery results) shares it.
func validateEndpointURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errcode.ErrOAuthProviderConfigInvalid
	}
	return nil
}

// requireNonEmpty rejects a trimmed-empty value for a field that create
// declared required — a sparse PATCH must not be able to blank it out.
func requireNonEmpty(v string) error {
	if strings.TrimSpace(v) == "" {
		return errcode.ErrOAuthProviderConfigInvalid
	}
	return nil
}

func (s *OAuthProviderService) ListProviders() ([]OAuthProviderView, error) {
	rows, err := repository.ListOAuthProviders(s.db)
	if err != nil {
		return nil, err
	}
	views := make([]OAuthProviderView, 0, len(rows))
	for _, p := range rows {
		count, err := repository.CountIdentitiesForProvider(s.db, p.ID)
		if err != nil {
			return nil, err
		}
		views = append(views, toOAuthProviderView(p, count))
	}
	return views, nil
}

func (s *OAuthProviderService) CreateProvider(in CreateOAuthProviderInput, now time.Time) (*OAuthProviderView, error) {
	// Same normalization the PATCH path applies: required text trims and
	// must stay non-empty (gin's `required` accepts pure whitespace),
	// endpoints must be absolute http(s) URLs.
	in.Name = strings.TrimSpace(in.Name)
	in.ClientID = strings.TrimSpace(in.ClientID)
	in.AuthorizationEndpoint = strings.TrimSpace(in.AuthorizationEndpoint)
	in.TokenEndpoint = strings.TrimSpace(in.TokenEndpoint)
	in.UserinfoEndpoint = strings.TrimSpace(in.UserinfoEndpoint)
	for _, required := range []string{in.Name, in.ClientID} {
		if err := requireNonEmpty(required); err != nil {
			return nil, err
		}
	}
	for _, endpoint := range []string{in.AuthorizationEndpoint, in.TokenEndpoint, in.UserinfoEndpoint} {
		if err := validateEndpointURL(endpoint); err != nil {
			return nil, err
		}
	}
	if err := validateProtocolKnobs(in.UserinfoTokenHeader, in.ExtraAuthorizeParams); err != nil {
		return nil, err
	}
	if _, err := repository.FindOAuthProviderBySlug(s.db, in.Slug); err == nil {
		return nil, errcode.ErrOAuthProviderSlugTaken
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	encrypted, err := s.secrets.Encrypt(in.ClientSecret)
	if err != nil {
		return nil, err
	}
	p := &model.OAuthProvider{
		Slug: in.Slug, Name: in.Name, Icon: in.Icon, Enabled: in.Enabled,
		ClientID: in.ClientID, EncryptedClientSecret: encrypted,
		AuthorizationEndpoint: in.AuthorizationEndpoint,
		TokenEndpoint:         in.TokenEndpoint,
		UserinfoEndpoint:      in.UserinfoEndpoint,
		Scopes:                fallbackScopes(in.Scopes),
		UserIDField:           fallback(in.UserIDField, "sub"),
		UsernameField:         fallback(in.UsernameField, "preferred_username"),
		DisplayNameField:      fallback(in.DisplayNameField, "name"),
		EmailField:            fallback(in.EmailField, "email"),
		AuthStyle:             normalizeAuthStyle(in.AuthStyle),
		OAuthProtocolStyle: model.OAuthProtocolStyle{
			TokenRequestStyle:    normalizeTokenRequestStyle(in.TokenRequestStyle),
			TokenFieldStyle:      normalizeTokenFieldStyle(in.TokenFieldStyle),
			UserinfoTokenHeader:  strings.TrimSpace(in.UserinfoTokenHeader),
			PkceEnabled:          in.PkceEnabled == nil || *in.PkceEnabled,
			ExtraAuthorizeParams: strings.TrimSpace(in.ExtraAuthorizeParams),
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repository.CreateOAuthProvider(s.db, p); err != nil {
		return nil, err
	}
	v := toOAuthProviderView(*p, 0)
	return &v, nil
}

// UpdateOAuthProviderInput is the sparse PATCH shape: nil leaves a field
// unchanged. ClientSecret nil = keep the stored secret; a non-nil value
// (including "") replaces it.
type UpdateOAuthProviderInput struct {
	Name                  *string
	Icon                  *string
	Enabled               *bool
	ClientID              *string
	ClientSecret          *string
	AuthorizationEndpoint *string
	TokenEndpoint         *string
	UserinfoEndpoint      *string
	Scopes                *string
	UserIDField           *string
	UsernameField         *string
	DisplayNameField      *string
	EmailField            *string
	AuthStyle             *string
	TokenRequestStyle     *string
	TokenFieldStyle       *string
	UserinfoTokenHeader   *string
	PkceEnabled           *bool
	ExtraAuthorizeParams  *string
}

func (s *OAuthProviderService) UpdateProvider(id uint, in UpdateOAuthProviderInput, now time.Time) (*OAuthProviderView, error) {
	existing, err := repository.FindOAuthProviderByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrOAuthProviderNotFound
		}
		return nil, err
	}
	// PATCH cannot weaken create-time invariants: required text fields
	// reject trimmed-empty replacements, endpoints must stay absolute
	// http(s) URLs.
	for _, required := range []*string{in.Name, in.ClientID} {
		if required != nil {
			if err := requireNonEmpty(*required); err != nil {
				return nil, err
			}
		}
	}
	for _, endpoint := range []*string{in.AuthorizationEndpoint, in.TokenEndpoint, in.UserinfoEndpoint} {
		if endpoint != nil {
			if err := validateEndpointURL(*endpoint); err != nil {
				return nil, err
			}
		}
	}
	updates := map[string]any{}
	setStr := func(col string, v *string) {
		if v != nil {
			updates[col] = strings.TrimSpace(*v)
		}
	}
	// The mapping fields and scopes fall back to the OIDC defaults on
	// PATCH exactly like on create: the edit form lets an admin clear
	// them, and persisting an empty user-id path would make every
	// subsequent login fail on an empty provider user id.
	setStrOrDefault := func(col string, v *string, def string) {
		if v != nil {
			updates[col] = fallback(*v, def)
		}
	}
	setStr("name", in.Name)
	setStr("icon", in.Icon)
	setStr("client_id", in.ClientID)
	setStr("authorization_endpoint", in.AuthorizationEndpoint)
	setStr("token_endpoint", in.TokenEndpoint)
	setStr("userinfo_endpoint", in.UserinfoEndpoint)
	// Scopes stores exactly what the PATCH carried, empty included: some
	// IdPs (Feishu) reject every OIDC-style scope name, and refilling a
	// default here would make those providers unfixable through the API.
	// The mapping fields below keep their refill — an empty user-id path
	// would break every login, an empty scope list is merely deliberate.
	if in.Scopes != nil {
		updates["scopes"] = strings.TrimSpace(*in.Scopes)
	}
	setStrOrDefault("user_id_field", in.UserIDField, "sub")
	setStrOrDefault("username_field", in.UsernameField, "preferred_username")
	setStrOrDefault("display_name_field", in.DisplayNameField, "name")
	setStrOrDefault("email_field", in.EmailField, "email")
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if in.AuthStyle != nil {
		updates["auth_style"] = normalizeAuthStyle(*in.AuthStyle)
	}
	// The free-form knobs validate against the merged (stored + patched)
	// shape, so a row whose stored extra_authorize_params violates the
	// reserved-key rule is caught on the next PATCH that touches either
	// knob — not only when this request happens to replace the offending
	// value.
	if in.UserinfoTokenHeader != nil || in.ExtraAuthorizeParams != nil {
		header, params := existing.UserinfoTokenHeader, existing.ExtraAuthorizeParams
		if in.UserinfoTokenHeader != nil {
			header = *in.UserinfoTokenHeader
		}
		if in.ExtraAuthorizeParams != nil {
			params = *in.ExtraAuthorizeParams
		}
		if err := validateProtocolKnobs(header, params); err != nil {
			return nil, err
		}
	}
	setStyle := func(col string, v *string, norm func(string) string) {
		if v != nil {
			updates[col] = norm(*v)
		}
	}
	setStyle("token_request_style", in.TokenRequestStyle, normalizeTokenRequestStyle)
	setStyle("token_field_style", in.TokenFieldStyle, normalizeTokenFieldStyle)
	setStr("userinfo_token_header", in.UserinfoTokenHeader)
	if in.PkceEnabled != nil {
		updates["pkce_enabled"] = *in.PkceEnabled
	}
	setStr("extra_authorize_params", in.ExtraAuthorizeParams)
	if in.ClientSecret != nil {
		encrypted, err := s.secrets.Encrypt(*in.ClientSecret)
		if err != nil {
			return nil, err
		}
		updates["encrypted_client_secret"] = encrypted
	}
	if err := repository.UpdateOAuthProvider(s.db, id, updates, now); err != nil {
		return nil, err
	}
	p, err := repository.FindOAuthProviderByID(s.db, id)
	if err != nil {
		return nil, err
	}
	count, err := repository.CountIdentitiesForProvider(s.db, id)
	if err != nil {
		return nil, err
	}
	v := toOAuthProviderView(*p, count)
	return &v, nil
}

// DeleteProvider removes a provider. Identities referencing it survive as
// rows (their accounts remain valid; they just lose this sign-in path) —
// the identity count is surfaced in the list view so the admin sees the
// blast radius before deleting.
func (s *OAuthProviderService) DeleteProvider(id uint) error {
	if _, err := repository.FindOAuthProviderByID(s.db, id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errcode.ErrOAuthProviderNotFound
		}
		return err
	}
	return repository.DeleteOAuthProvider(s.db, id)
}

// OIDCDiscoveryResult carries the endpoints the admin form auto-fills
// from a provider's well-known document.
type OIDCDiscoveryResult struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// Discover fetches an OIDC discovery document. Accepts either the full
// .well-known URL or a bare issuer URL (the standard path is appended).
func (s *OAuthProviderService) Discover(ctx context.Context, rawURL string) (*OIDCDiscoveryResult, error) {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return nil, errcode.ErrOAuthDiscoveryFailed
	}
	if !strings.Contains(u, "/.well-known/") {
		u = strings.TrimRight(u, "/") + "/.well-known/openid-configuration"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errcode.ErrOAuthDiscoveryFailed
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrOAuthDiscoveryFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", errcode.ErrOAuthDiscoveryFailed, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrOAuthDiscoveryFailed, err)
	}
	var doc OIDCDiscoveryResult
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrOAuthDiscoveryFailed, err)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return nil, errcode.ErrOAuthDiscoveryFailed
	}
	// A malicious or broken discovery document must not smuggle a
	// non-http(s) endpoint past the same rule create/update enforce.
	for _, endpoint := range []string{doc.AuthorizationEndpoint, doc.TokenEndpoint} {
		if err := validateEndpointURL(endpoint); err != nil {
			return nil, errcode.ErrOAuthDiscoveryFailed
		}
	}
	if doc.UserinfoEndpoint != "" {
		if err := validateEndpointURL(doc.UserinfoEndpoint); err != nil {
			return nil, errcode.ErrOAuthDiscoveryFailed
		}
	}
	return &doc, nil
}
