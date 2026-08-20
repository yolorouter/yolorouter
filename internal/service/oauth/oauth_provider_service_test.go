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
