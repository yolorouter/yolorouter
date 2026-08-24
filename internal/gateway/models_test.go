package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// newModelsGetCtx builds a GET gin.Context for /v1/models tests. anthropic
// =true sets the Anthropic SDK's anthropic-version header so
// IngressProtocolForContext picks the Anthropic envelope.
func newModelsGetCtx(target string, anthropic bool) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	if anthropic {
		c.Request.Header.Set("anthropic-version", "2023-06-01")
	}
	return c, w
}

func TestOpenAIModelObject(t *testing.T) {
	obj := openAIModelObject(model.Model{Name: "gpt-4o", CreatedAt: time.Unix(1700000000, 0)})
	if obj["id"] != "gpt-4o" || obj["object"] != "model" || obj["owned_by"] != ownedByTag {
		t.Errorf("obj=%v", obj)
	}
	if obj["created"] != int64(1700000000) {
		t.Errorf("created=%v want 1700000000", obj["created"])
	}
}

func TestAnthropicModelObject(t *testing.T) {
	obj := anthropicModelObject(model.Model{Name: "claude-3", CreatedAt: time.Unix(1700000000, 0).UTC()})
	if obj["type"] != "model" || obj["id"] != "claude-3" || obj["display_name"] != "claude-3" {
		t.Errorf("obj=%v", obj)
	}
	created, ok := obj["created_at"].(string)
	if !ok {
		t.Fatalf("created_at not string: %T", obj["created_at"])
	}
	if _, err := time.Parse(time.RFC3339, created); err != nil {
		t.Errorf("created_at %q not RFC3339: %v", created, err)
	}
}

func TestWriteModelListOpenAI(t *testing.T) {
	c, w := newModelsGetCtx("/v1/models", false)
	writeModelList(c, protocols.ProtocolOpenAI, []model.Model{
		{Name: "a", CreatedAt: time.Unix(1, 0)}, {Name: "b", CreatedAt: time.Unix(2, 0)},
	})
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if body["object"] != "list" {
		t.Errorf("object=%v want list", body["object"])
	}
	if data, _ := body["data"].([]any); len(data) != 2 {
		t.Fatalf("data len=%d want 2", len(data))
	}
}

func TestWriteModelListAnthropicEmpty(t *testing.T) {
	c, w := newModelsGetCtx("/v1/models", true)
	writeModelList(c, protocols.ProtocolClaude, nil)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if body["has_more"] != false {
		t.Errorf("has_more=%v want false", body["has_more"])
	}
	if data, _ := body["data"].([]any); len(data) != 0 {
		t.Errorf("data len=%d want 0", len(data))
	}
	// Anthropic's empty-list cursor fields are null, not "".
	if body["first_id"] != nil || body["last_id"] != nil {
		t.Errorf("first/last=%v/%v want nil", body["first_id"], body["last_id"])
	}
}

func TestRejectInvalidKey(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	const rid = "test-rid"

	c, w := newModelsGetCtx("/v1/models", false)
	if !rejectInvalidKey(c, protocols.ProtocolOpenAI, &model.APIKey{Status: model.APIKeyStatusRevoked}, rid) {
		t.Fatal("revoked key not rejected")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked: status=%d want 401", w.Code)
	}

	c2, w2 := newModelsGetCtx("/v1/models", false)
	if !rejectInvalidKey(c2, protocols.ProtocolOpenAI, &model.APIKey{Status: model.APIKeyStatusActive, ExpiresAt: &past}, rid) {
		t.Fatal("expired key not rejected")
	}
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("expired: status=%d want 401", w2.Code)
	}

	c3, _ := newModelsGetCtx("/v1/models", false)
	if rejectInvalidKey(c3, protocols.ProtocolOpenAI, &model.APIKey{Status: model.APIKeyStatusActive}, rid) {
		t.Fatal("active key incorrectly rejected")
	}
}

func seedModel(t *testing.T, db *gorm.DB, name string, enabled bool) *model.Model {
	t.Helper()
	now := time.Now().UTC()
	status := model.ModelStatusEnabled
	if !enabled {
		status = model.ModelStatusDisabled
	}
	m := &model.Model{Name: name, ManagementStatus: status, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	return m
}

func TestListModels_AllowAllModels(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedModel(t, db, "bravo", true)
	seedModel(t, db, "alpha", true)
	seedModel(t, db, "hidden", false) // disabled -> dropped

	k := createAPIKey(t, db, model.APIKeyStatusActive, nil)
	if err := db.Model(&model.APIKey{}).Where("id = ?", k.ID).Update("allow_all_models", true).Error; err != nil {
		t.Fatalf("set allow_all_models: %v", err)
	}
	// Sync the in-memory key: SetGatewayAuth stores this pointer directly,
	// skipping the DB reload APIKeyAuth performs in production.
	k.AllowAllModels = true

	c, w := newModelsGetCtx("/v1/models", false)
	SetGatewayAuth(c, k)
	ListModels(db)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, _ := body["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data len=%d want 2 (enabled only)", len(data))
	}
	if data[0].(map[string]any)["id"] != "alpha" {
		t.Errorf("first id=%v want alpha (name-sorted)", data[0].(map[string]any)["id"])
	}
}

func TestListModels_AllowlistFilter(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m1 := seedModel(t, db, "alpha", true)
	seedModel(t, db, "bravo", true) // not in allowlist

	k := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m1.ID})

	c, w := newModelsGetCtx("/v1/models", false)
	SetGatewayAuth(c, k)
	ListModels(db)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, _ := body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["id"] != "alpha" {
		t.Fatalf("data=%v want only [alpha]", data)
	}
}

func TestListModels_EmptyAllowlist(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedModel(t, db, "alpha", true)
	k := createAPIKey(t, db, model.APIKeyStatusActive, nil) // empty allowlist, AllowAllModels=false

	c, w := newModelsGetCtx("/v1/models", false)
	SetGatewayAuth(c, k)
	ListModels(db)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, _ := body["data"].([]any)
	if len(data) != 0 {
		t.Fatalf("data len=%d want 0 (empty allowlist permits nothing)", len(data))
	}
}

func TestListModels_AnthropicEnvelope(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedModel(t, db, "alpha", true)
	k := createAPIKey(t, db, model.APIKeyStatusActive, nil)
	if err := db.Model(&model.APIKey{}).Where("id = ?", k.ID).Update("allow_all_models", true).Error; err != nil {
		t.Fatalf("set allow_all_models: %v", err)
	}
	// Sync the in-memory key: SetGatewayAuth stores this pointer directly,
	// skipping the DB reload APIKeyAuth performs in production.
	k.AllowAllModels = true

	c, w := newModelsGetCtx("/v1/models", true) // anthropic-version header
	SetGatewayAuth(c, k)
	ListModels(db)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["has_more"] != false || body["first_id"] != "alpha" || body["last_id"] != "alpha" {
		t.Errorf("list meta=%v", body)
	}
	obj := body["data"].([]any)[0].(map[string]any)
	if obj["type"] != "model" || obj["display_name"] != "alpha" {
		t.Errorf("anthropic obj=%v", obj)
	}
}

func TestListModels_RevokedKey(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedModel(t, db, "alpha", true)
	k := createAPIKey(t, db, model.APIKeyStatusRevoked, nil)

	c, w := newModelsGetCtx("/v1/models", false)
	SetGatewayAuth(c, k)
	ListModels(db)(c)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
}

func TestRetrieveModel_Found(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m := seedModel(t, db, "alpha", true)
	k := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newModelsGetCtx("/v1/models/alpha", false)
	c.Params = gin.Params{{Key: "model", Value: "alpha"}}
	SetGatewayAuth(c, k)
	RetrieveModel(db)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if obj["id"] != "alpha" || obj["object"] != "model" {
		t.Errorf("obj=%v want id=alpha object=model", obj)
	}
}

func TestRetrieveModel_NotFound(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	k := createAPIKey(t, db, model.APIKeyStatusActive, nil)
	if err := db.Model(&model.APIKey{}).Where("id = ?", k.ID).Update("allow_all_models", true).Error; err != nil {
		t.Fatalf("set allow_all_models: %v", err)
	}
	// Sync the in-memory key: SetGatewayAuth stores this pointer directly,
	// skipping the DB reload APIKeyAuth performs in production.
	k.AllowAllModels = true

	c, w := newModelsGetCtx("/v1/models/nope", false)
	c.Params = gin.Params{{Key: "model", Value: "nope"}}
	SetGatewayAuth(c, k)
	RetrieveModel(db)(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestRetrieveModel_Disabled(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m := seedModel(t, db, "alpha", false)
	k := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newModelsGetCtx("/v1/models/alpha", false)
	c.Params = gin.Params{{Key: "model", Value: "alpha"}}
	SetGatewayAuth(c, k)
	RetrieveModel(db)(c)

	// Disabled reports "does not exist" (no existence leak), matching the relay.
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestRetrieveModel_NotInAllowlist(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedModel(t, db, "alpha", true)
	k := createAPIKey(t, db, model.APIKeyStatusActive, nil) // empty allowlist

	c, w := newModelsGetCtx("/v1/models/alpha", false)
	c.Params = gin.Params{{Key: "model", Value: "alpha"}}
	SetGatewayAuth(c, k)
	RetrieveModel(db)(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", w.Code)
	}
}

func TestWriteModelObject(t *testing.T) {
	m := model.Model{Name: "gpt-4o", CreatedAt: time.Unix(1700000000, 0).UTC()}

	t.Run("openai", func(t *testing.T) {
		c, w := newModelsGetCtx("/v1/models/gpt-4o", false)
		writeModelObject(c, protocols.ProtocolOpenAI, m)
		var obj map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		if obj["id"] != "gpt-4o" || obj["object"] != "model" {
			t.Errorf("obj=%v want id=gpt-4o object=model", obj)
		}
		if obj["owned_by"] != ownedByTag {
			t.Errorf("owned_by=%v want %v", obj["owned_by"], ownedByTag)
		}
		// json.Unmarshal produces float64 for JSON numbers in map[string]any.
		if obj["created"] != float64(1700000000) {
			t.Errorf("created=%v want 1700000000", obj["created"])
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		c, w := newModelsGetCtx("/v1/models/gpt-4o", true)
		writeModelObject(c, protocols.ProtocolClaude, m)
		var obj map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
		}
		if obj["type"] != "model" || obj["id"] != "gpt-4o" || obj["display_name"] != "gpt-4o" {
			t.Errorf("obj=%v want type=model id=gpt-4o display_name=gpt-4o", obj)
		}
		created, ok := obj["created_at"].(string)
		if !ok {
			t.Fatalf("created_at not string: %T", obj["created_at"])
		}
		if _, err := time.Parse(time.RFC3339, created); err != nil {
			t.Errorf("created_at %q not RFC3339: %v", created, err)
		}
	})
}

// The route uses a catch-all parameter so slash-namespaced ids match, which
// makes gin hand the handler the name with a leading "/" — the handler must
// strip it before the lookup.
func TestRetrieveModel_SlashNamedModelViaCatchAllParam(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m := seedModel(t, db, "deepseek-ai/DeepSeek-V4", true)
	k := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newModelsGetCtx("/v1/models/deepseek-ai/DeepSeek-V4", false)
	c.Params = gin.Params{{Key: "model", Value: "/deepseek-ai/DeepSeek-V4"}}
	SetGatewayAuth(c, k)
	RetrieveModel(db)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if obj["id"] != "deepseek-ai/DeepSeek-V4" {
		t.Errorf("obj=%v want id=deepseek-ai/DeepSeek-V4", obj)
	}
}

// The old ":model" route relied on gin's trailing-slash redirect; the
// catch-all matches "/alpha/" directly, so the handler must ignore trailing
// slashes itself to keep that behavior.
func TestRetrieveModel_TrailingSlashResolvesModel(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m := seedModel(t, db, "alpha", true)
	k := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newModelsGetCtx("/v1/models/alpha/", false)
	c.Params = gin.Params{{Key: "model", Value: "/alpha/"}}
	SetGatewayAuth(c, k)
	RetrieveModel(db)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["id"] != "alpha" {
		t.Errorf("obj=%v want id=alpha", obj)
	}
}

// "/v1/models/" reaches the catch-all with an empty (all-slash) name; the old
// route redirected that to the list endpoint, so the handler falls back to the
// same listing instead of a bogus not-found.
func TestRetrieveModel_EmptyNameFallsBackToList(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	m := seedModel(t, db, "alpha", true)
	k := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newModelsGetCtx("/v1/models/", false)
	c.Params = gin.Params{{Key: "model", Value: "/"}}
	SetGatewayAuth(c, k)
	RetrieveModel(db)(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["object"] != "list" {
		t.Errorf("obj=%v want object=list", obj)
	}
}
