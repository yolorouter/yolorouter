package handler

import (
	"encoding/json"
	"net/http/httptest"

	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// setupRateLimitsRouter builds the two admin endpoints over a raw sqlite
// database with the tables the join needs — the endpoints are thin reads
// over the repository, so the contract worth pinning is the response
// envelope, not storage (which its own tests cover).
func setupRateLimitsRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE providers (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL DEFAULT '')`)
	db.Exec(`CREATE TABLE provider_keys (id INTEGER PRIMARY KEY AUTOINCREMENT, provider_id INTEGER NOT NULL, label TEXT NOT NULL DEFAULT '')`)
	db.Exec(`CREATE TABLE observed_rate_limits (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider_key_id INTEGER NOT NULL,
		meter TEXT NOT NULL,
		limit_value INTEGER NULL,
		window_seconds INTEGER NULL,
		last_remaining INTEGER NULL,
		last_reset_at DATETIME NULL,
		observed_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE (provider_key_id, meter))`)
	at := time.Now().UTC()
	db.Exec(`INSERT INTO providers (name) VALUES ('p1')`)
	db.Exec(`INSERT INTO provider_keys (provider_id, label) VALUES (1, 'k1')`)
	if err := repository.UpsertObservedRateLimit(db, 1, model.MeterRequests, func(v int64) *int64 { return &v }(60), nil, nil, nil, at); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := gin.New()
	r.GET("/api/admin/rate-limits", GetRateLimits(db))
	r.DELETE("/api/admin/rate-limits/:id", DeleteRateLimit(db))
	return r
}

func TestGetRateLimitsReturnsJoinedRows(t *testing.T) {
	r := setupRateLimitsRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/rate-limits", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			List []repository.ObservedRateLimitView `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.List) != 1 {
		t.Fatalf("list = %+v, want one row", resp.Data.List)
	}
	row := resp.Data.List[0]
	if row.ProviderName != "p1" || row.KeyLabel != "k1" || row.Meter != "requests" {
		t.Fatalf("row = %+v, want joined names and requests meter", row)
	}
	if row.LimitValue == nil || *row.LimitValue != 60 {
		t.Fatalf("limit = %v, want 60", row.LimitValue)
	}
}

func TestDeleteRateLimitSucceedsAndIsIdempotent(t *testing.T) {
	r := setupRateLimitsRouter(t)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("DELETE", "/api/admin/rate-limits/1", nil))
		if w.Code != 200 {
			t.Fatalf("delete %d: status = %d, body = %s", i+1, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/rate-limits", nil))
	if w.Code != 200 {
		t.Fatalf("get: status = %d", w.Code)
	}
	var after struct {
		Data struct {
			List []repository.ObservedRateLimitView `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.Data.List == nil || len(after.Data.List) != 0 {
		t.Fatalf("after delete list = %+v, want empty (and non-null)", after.Data.List)
	}
}
