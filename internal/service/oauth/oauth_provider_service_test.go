package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func newOAuthProviderServiceForTest(t *testing.T) (*OAuthProviderService, *OAuthLoginService) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	return NewOAuthProviderService(db, testutil.ProviderSecrets()), NewOAuthLoginService(db, testutil.ProviderSecrets())
}

func minimalProviderInput(slug string) CreateOAuthProviderInput {
	return CreateOAuthProviderInput{
		Slug: slug, Name: "Test IdP", Enabled: true,
		ClientID: "cid", ClientSecret: "very-secret",
		AuthorizationEndpoint: "https://idp.example.com/authorize",
		TokenEndpoint:         "https://idp.example.com/token",
		UserinfoEndpoint:      "https://idp.example.com/userinfo",
	}
}

func TestCreateOAuthProviderEncryptsSecretAndDefaultsMappings(t *testing.T) {
	svc, _ := newOAuthProviderServiceForTest(t)
	now := time.Now().UTC()

	view, err := svc.CreateProvider(minimalProviderInput("zitadel"), now)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if !view.HasClientSecret {
		t.Fatalf("view should report a stored secret")
	}
	// Blank optional fields fall back to the OIDC defaults.
	if view.Scopes != "openid profile email" || view.UserIDField != "sub" ||
		view.UsernameField != "preferred_username" || view.EmailField != "email" {
		t.Fatalf("OIDC defaults not applied: %+v", view)
	}

	// The stored secret must be ciphertext that decrypts back, never the
	// plaintext itself.
	var stored model.OAuthProvider
	if err := svc.db.First(&stored, view.ID).Error; err != nil {
		t.Fatalf("load stored: %v", err)
	}
	if stored.EncryptedClientSecret == "very-secret" || stored.EncryptedClientSecret == "" {
		t.Fatalf("secret stored as plaintext or empty")
	}
	plain, err := crypto.Decrypt(testutil.ProviderMasterKey(), stored.EncryptedClientSecret)
	if err != nil || plain != "very-secret" {
		t.Fatalf("ciphertext does not decrypt back: %q %v", plain, err)
	}
}

// TestOAuthProviderTokenStyleNormalization: the protocol-style columns
// default to the standard shape on create, persist explicit non-standard
// values, and normalize on PATCH — a provider that never opted into a
// JSON style must keep the form-encoded exchange.
func TestOAuthProviderTokenStyleNormalization(t *testing.T) {
	svc, _ := newOAuthProviderServiceForTest(t)
	now := time.Now().UTC()

	view, err := svc.CreateProvider(minimalProviderInput("standard"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if view.TokenRequestStyle != model.OAuthTokenRequestStyleForm || view.TokenFieldStyle != model.OAuthTokenFieldStyleSnake {
		t.Fatalf("create must default to form/snake, got %s/%s", view.TokenRequestStyle, view.TokenFieldStyle)
	}

	jsonStyle := model.OAuthTokenRequestStyleJSON
	camelStyle := model.OAuthTokenFieldStyleCamel
	view, err = svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{
		TokenRequestStyle: &jsonStyle, TokenFieldStyle: &camelStyle,
	}, now)
	if err != nil {
		t.Fatalf("patch styles: %v", err)
	}
	if view.TokenRequestStyle != model.OAuthTokenRequestStyleJSON || view.TokenFieldStyle != model.OAuthTokenFieldStyleCamel {
		t.Fatalf("patch should persist json/camel, got %s/%s", view.TokenRequestStyle, view.TokenFieldStyle)
	}

	// A style-less PATCH leaves the configured values alone (sparse PATCH),
	// while an explicit write normalizes anything unrecognized back to the
	// standard shape.
	view, err = svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{}, now)
	if err != nil {
		t.Fatalf("empty patch: %v", err)
	}
	if view.TokenRequestStyle != model.OAuthTokenRequestStyleJSON || view.TokenFieldStyle != model.OAuthTokenFieldStyleCamel {
		t.Fatalf("empty patch must not touch the styles, got %s/%s", view.TokenRequestStyle, view.TokenFieldStyle)
	}
	garbage := "xml"
	view, err = svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{TokenRequestStyle: &garbage}, now)
	if err != nil {
		t.Fatalf("garbage patch: %v", err)
	}
	if view.TokenRequestStyle != model.OAuthTokenRequestStyleForm {
		t.Fatalf("unrecognized style must normalize to form, got %s", view.TokenRequestStyle)
	}
}

// TestOAuthProviderDingTalkKnobsValidation: the three DingTalk columns
// default on create (PKCE on, no custom header, no extra params), reject
// invalid values, and persist through PATCH.
func TestOAuthProviderDingTalkKnobsValidation(t *testing.T) {
	svc, _ := newOAuthProviderServiceForTest(t)
	now := time.Now().UTC()

	view, err := svc.CreateProvider(minimalProviderInput("knobs"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !view.PkceEnabled || view.UserinfoTokenHeader != "" || view.ExtraAuthorizeParams != "" {
		t.Fatalf("create defaults: pkce on, no header, no params — got %+v", view)
	}

	// Reserved keys and malformed query strings are rejected at write time.
	reserved := "prompt=consent&state=evil"
	if _, err := svc.CreateProvider(withKnobs(minimalProviderInput("reserved"), "x-h", true, reserved), now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
		t.Fatalf("reserved key must be rejected, got %v", err)
	}
	if _, err := svc.CreateProvider(withKnobs(minimalProviderInput("broken"), "x-h", true, "%zz"), now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
		t.Fatalf("malformed query must be rejected, got %v", err)
	}
	// The header name must be an RFC 7230 token.
	for _, bad := range []string{"x h", "x:h", "xéh"} {
		if _, err := svc.CreateProvider(withKnobs(minimalProviderInput("badh"), bad, true, ""), now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
			t.Fatalf("header name %q must be rejected, got %v", bad, err)
		}
	}

	// The DingTalk preset shape persists end to end.
	prompt := "prompt=consent"
	header := "x-acs-dingtalk-access-token"
	off := false
	view, err = svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{
		UserinfoTokenHeader: &header, PkceEnabled: &off, ExtraAuthorizeParams: &prompt,
	}, now)
	if err != nil {
		t.Fatalf("patch knobs: %v", err)
	}
	if view.PkceEnabled || view.UserinfoTokenHeader != header || view.ExtraAuthorizeParams != prompt {
		t.Fatalf("patched knobs: %+v", view)
	}

	// PATCH with the same garbage is rejected too.
	badParams := "scope=openid"
	if _, err := svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{ExtraAuthorizeParams: &badParams}, now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
		t.Fatalf("patch with reserved key must be rejected, got %v", err)
	}
	badHeader := "not a header"
	if _, err := svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{UserinfoTokenHeader: &badHeader}, now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
		t.Fatalf("patch with bad header must be rejected, got %v", err)
	}
}

// TestOAuthProviderPatchRevalidatesStoredKnobs pins the merged validation:
// a row whose stored extra params violate the reserved-key rule — only
// reachable by direct DB writes, every API path validates — is caught the
// next time a PATCH touches either free-form knob, not just when the
// offending value itself is replaced.
func TestOAuthProviderPatchRevalidatesStoredKnobs(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewOAuthProviderService(db, testutil.ProviderSecrets())
	now := time.Now().UTC()

	view, err := svc.CreateProvider(minimalProviderInput("storedbad"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Model(&model.OAuthProvider{}).Where("id = ?", view.ID).
		Update("extra_authorize_params", "scope=evil").Error; err != nil {
		t.Fatalf("seed stored bad params: %v", err)
	}
	header := "x-custom-token"
	if _, err := svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{UserinfoTokenHeader: &header}, now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
		t.Fatalf("patch touching a knob must revalidate the stored pair, got %v", err)
	}
}

// withKnobs returns a copy of in with the three DingTalk knobs set, so
// each invalid-value case starts from an otherwise valid input.
func withKnobs(in CreateOAuthProviderInput, header string, pkce bool, params string) CreateOAuthProviderInput {
	in.UserinfoTokenHeader = header
	in.PkceEnabled = &pkce
	in.ExtraAuthorizeParams = params
	return in
}

// TestOAuthProviderScopesAreExplicit pins the scopes semantics: nil takes
// the OIDC default (a request that never mentioned scopes), an explicit
// empty is a stored choice (Feishu's authorize endpoint rejects OIDC-style
// scope names with error 20043), and PATCH can clear the field without a
// refill.
func TestOAuthProviderScopesAreExplicit(t *testing.T) {
	svc, _ := newOAuthProviderServiceForTest(t)
	now := time.Now().UTC()

	view, err := svc.CreateProvider(minimalProviderInput("scopes"), now)
	if err != nil {
		t.Fatalf("create without scopes: %v", err)
	}
	if view.Scopes != "openid profile email" {
		t.Fatalf("unmentioned scopes must default, got %q", view.Scopes)
	}

	empty := ""
	view, err = svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{Scopes: &empty}, now)
	if err != nil {
		t.Fatalf("patch scopes empty: %v", err)
	}
	if view.Scopes != "" {
		t.Fatalf("explicit empty scopes must stay empty (no refill), got %q", view.Scopes)
	}

	custom := "contact:user.base:readonly"
	view, err = svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{Scopes: &custom}, now)
	if err != nil {
		t.Fatalf("patch scopes custom: %v", err)
	}
	if view.Scopes != custom {
		t.Fatalf("custom scopes must persist, got %q", view.Scopes)
	}
	view, err = svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{}, now)
	if err != nil {
		t.Fatalf("empty patch: %v", err)
	}
	if view.Scopes != custom {
		t.Fatalf("scopes-less patch must not touch scopes, got %q", view.Scopes)
	}

	// Create with an explicit empty (the Feishu preset's shape) stores empty.
	in := minimalProviderInput("feishu-like")
	in.Scopes = &empty
	view, err = svc.CreateProvider(in, now)
	if err != nil {
		t.Fatalf("create with empty scopes: %v", err)
	}
	if view.Scopes != "" {
		t.Fatalf("create with explicit empty scopes must store empty, got %q", view.Scopes)
	}
}

func TestCreateOAuthProviderRejectsDuplicateSlug(t *testing.T) {
	svc, _ := newOAuthProviderServiceForTest(t)
	now := time.Now().UTC()
	if _, err := svc.CreateProvider(minimalProviderInput("dup"), now); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.CreateProvider(minimalProviderInput("dup"), now)
	if !errors.Is(err, errcode.ErrOAuthProviderSlugTaken) {
		t.Fatalf("expected ErrOAuthProviderSlugTaken, got %v", err)
	}
}

// TestUpdateOAuthProviderKeepsSecretWhenOmitted pins the write-only secret
// contract: a PATCH without client_secret must leave the stored ciphertext
// untouched, while supplying one replaces it.
func TestUpdateOAuthProviderKeepsSecretWhenOmitted(t *testing.T) {
	svc, _ := newOAuthProviderServiceForTest(t)
	now := time.Now().UTC()
	view, err := svc.CreateProvider(minimalProviderInput("keep"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	name := "Renamed"
	if _, err := svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{Name: &name}, now); err != nil {
		t.Fatalf("update without secret: %v", err)
	}
	var stored model.OAuthProvider
	if err := svc.db.First(&stored, view.ID).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if plain, err := crypto.Decrypt(testutil.ProviderMasterKey(), stored.EncryptedClientSecret); err != nil || plain != "very-secret" {
		t.Fatalf("secret changed by a secretless PATCH: %q %v", plain, err)
	}

	newSecret := "rotated"
	if _, err := svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{ClientSecret: &newSecret}, now); err != nil {
		t.Fatalf("update with secret: %v", err)
	}
	if err := svc.db.First(&stored, view.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if plain, err := crypto.Decrypt(testutil.ProviderMasterKey(), stored.EncryptedClientSecret); err != nil || plain != "rotated" {
		t.Fatalf("secret not rotated: %q %v", plain, err)
	}
}

// TestPublicProviderListExposesOnlyButtonFields: the login page's list
// must never carry credentials or endpoints, and must omit disabled
// providers entirely.
func TestPublicProviderListExposesOnlyButtonFields(t *testing.T) {
	svc, loginSvc := newOAuthProviderServiceForTest(t)
	now := time.Now().UTC()
	if _, err := svc.CreateProvider(minimalProviderInput("visible"), now); err != nil {
		t.Fatalf("create: %v", err)
	}
	off := minimalProviderInput("hidden")
	off.Enabled = false
	if _, err := svc.CreateProvider(off, now); err != nil {
		t.Fatalf("create disabled: %v", err)
	}

	views, err := loginSvc.ListEnabledProviders()
	if err != nil {
		t.Fatalf("ListEnabledProviders: %v", err)
	}
	if len(views) != 1 || views[0].Slug != "visible" {
		t.Fatalf("expected only the enabled provider, got %+v", views)
	}
	// Serialize and prove the shape carries nothing sensitive.
	raw, err := json.Marshal(views[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"client_id", "client_secret", "authorization_endpoint", "token_endpoint", "userinfo_endpoint"} {
		if _, ok := asMap[forbidden]; ok {
			t.Fatalf("public view leaks %q: %s", forbidden, raw)
		}
	}
}

// TestOAuthProviderRejectsNonHTTPEndpoints pins the scheme guard: a
// javascript: authorization endpoint would execute in the login page's
// origin (the frontend navigates to the URL the server returns), so
// create, patch, and discovery must all refuse anything but http(s).
func TestOAuthProviderRejectsNonHTTPEndpoints(t *testing.T) {
	svc, _ := newOAuthProviderServiceForTest(t)
	now := time.Now().UTC()

	bad := minimalProviderInput("evil")
	bad.AuthorizationEndpoint = "javascript:alert(1)//"
	if _, err := svc.CreateProvider(bad, now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
		t.Fatalf("create with javascript: endpoint must be rejected, got %v", err)
	}

	view, err := svc.CreateProvider(minimalProviderInput("good"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	evil := "javascript:alert(1)//"
	if _, err := svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{AuthorizationEndpoint: &evil}, now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
		t.Fatalf("patch to javascript: endpoint must be rejected, got %v", err)
	}
}

// TestOAuthProviderPatchRejectsBlankRequiredFields: a sparse PATCH must
// not blank out fields create declared required, nor clear the user-id
// mapping to an empty path (which would fail every subsequent login).
func TestOAuthProviderPatchRejectsBlankRequiredFields(t *testing.T) {
	svc, _ := newOAuthProviderServiceForTest(t)
	now := time.Now().UTC()
	view, err := svc.CreateProvider(minimalProviderInput("patched"), now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	empty := "  "
	if _, err := svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{Name: &empty}, now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
		t.Fatalf("blank name must be rejected, got %v", err)
	}
	if _, err := svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{ClientID: &empty}, now); !errors.Is(err, errcode.ErrOAuthProviderConfigInvalid) {
		t.Fatalf("blank client_id must be rejected, got %v", err)
	}

	// The mapping fields fall back to OIDC defaults instead of persisting
	// an empty path.
	updated, err := svc.UpdateProvider(view.ID, UpdateOAuthProviderInput{UserIDField: &empty}, now)
	if err != nil {
		t.Fatalf("patch with empty user_id_field should fall back, got %v", err)
	}
	if updated.UserIDField != "sub" {
		t.Fatalf("expected user_id_field to fall back to sub, got %q", updated.UserIDField)
	}
}

func TestOAuthDiscoverAcceptsIssuerAndWellKnownURLs(t *testing.T) {
	doc := map[string]string{
		"authorization_endpoint": "https://idp.example.com/authorize",
		"token_endpoint":         "https://idp.example.com/token",
		"userinfo_endpoint":      "https://idp.example.com/userinfo",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer server.Close()

	svc, _ := newOAuthProviderServiceForTest(t)
	for _, u := range []string{server.URL, server.URL + "/.well-known/openid-configuration"} {
		result, err := svc.Discover(context.Background(), u)
		if err != nil {
			t.Fatalf("Discover(%q): %v", u, err)
		}
		if result.TokenEndpoint != doc["token_endpoint"] {
			t.Fatalf("unexpected discovery result: %+v", result)
		}
	}

	if _, err := svc.Discover(context.Background(), server.URL+"/nope/.well-known/openid-configuration"); !errors.Is(err, errcode.ErrOAuthDiscoveryFailed) {
		t.Fatalf("expected discovery failure, got %v", err)
	}
}
