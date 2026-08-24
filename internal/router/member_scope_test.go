package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// memberScopeFixture builds a full router with two members and an admin,
// each holding a live session, plus one API key and one request-log row
// per member — the minimal world in which every cross-account reach is
// observable.
type memberScopeFixture struct {
	r          *gin.Engine
	db         *gorm.DB
	adminCk    *http.Cookie
	aliceCk    *http.Cookie
	aliceID    uint
	aliceKeyID uint
	bobKeyID   uint
}

func newMemberScopeFixture(t *testing.T) *memberScopeFixture {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	r, err := New(testDeps(t, db))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now().UTC()

	seed := func(username, role string, isLocal, isBootstrap bool, token string) *model.User {
		u := &model.User{Username: username, Role: role, Status: model.UserStatusEnabled, IsLocal: isLocal,
			IsBootstrap: isBootstrap, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
		if err := repository.CreateUser(db, u); err != nil {
			t.Fatalf("seed %s: %v", username, err)
		}
		if err := repository.CreateSession(db, token, u.ID, now.Add(time.Hour), now); err != nil {
			t.Fatalf("session %s: %v", username, err)
		}
		return u
	}
	// boss mirrors first-run setup: the one bootstrap local admin.
	admin := seed("boss", model.RoleAdmin, true, true, "tok-admin")
	alice := seed("alice", model.RoleMember, false, false, "tok-alice")
	bob := seed("bob", model.RoleMember, false, false, "tok-bob")
	_ = admin

	seedKey := func(owner uint, hash string) uint {
		k := &model.APIKey{KeyHash: hash, KeyPrefix: "sk-yr-" + hash[:4], UserID: owner,
			Status: model.APIKeyStatusActive, AllowAllModels: true, CreatedAt: now, UpdatedAt: now}
		if err := repository.CreateAPIKey(db, k, nil, now); err != nil {
			t.Fatalf("seed key %s: %v", hash, err)
		}
		return k.ID
	}
	aliceKey := seedKey(alice.ID, "hash-alice")
	bobKey := seedKey(bob.ID, "hash-bob")

	// Two providers back the request logs' provider identities (the
	// provider_id column is foreign-keyed): 5 serves alice, 7 serves bob.
	for _, pid := range []uint{5, 7} {
		provider := &model.Provider{ID: pid, Name: "p" + uintToString(pid), ProviderType: "openai",
			BaseURL: "https://upstream.example", CreatedAt: now, UpdatedAt: now}
		if err := db.Create(provider).Error; err != nil {
			t.Fatalf("seed provider %d: %v", pid, err)
		}
	}

	seedLog := func(owner, keyID uint, rid string, cost int64, providerID uint, status int) {
		row := &model.RequestLog{RequestID: rid, APIKeyID: &keyID, UserID: &owner,
			ModelName: "m", StatusCode: status, ProviderID: &providerID,
			CostMicros: cost, CostKnown: true, CreatedAt: now}
		if err := repository.CreateRequestLog(db, row); err != nil {
			t.Fatalf("seed log %s: %v", rid, err)
		}
	}
	seedLog(alice.ID, aliceKey, "req-alice", 100, 5, 200)
	seedLog(bob.ID, bobKey, "req-bob", 900, 7, 200)
	// A failed request for alice, carrying a provider identity — the member
	// dashboard's recent-failures list must strip it.
	seedLog(alice.ID, aliceKey, "req-alice-fail", 0, 5, 502)

	return &memberScopeFixture{
		r: r, db: db,
		adminCk:    &http.Cookie{Name: "session_id", Value: "tok-admin"},
		aliceCk:    &http.Cookie{Name: "session_id", Value: "tok-alice"},
		aliceID:    alice.ID,
		aliceKeyID: aliceKey,
		bobKeyID:   bobKey,
	}
}

func (f *memberScopeFixture) do(t *testing.T, method, path string, body string, ck *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

// TestMemberKeyAccessIsOwnerScoped: list shows only own keys regardless
// of the user_id filter, and every by-id operation on another member's
// key answers 404 — indistinguishable from a key that never existed.
func TestMemberKeyAccessIsOwnerScoped(t *testing.T) {
	f := newMemberScopeFixture(t)

	// List, even explicitly asking for bob's keys, returns only alice's.
	w := f.do(t, http.MethodGet, "/api/admin/api-keys?q=&owner=&status=&page=1&page_size=50&user_id=0", "", f.aliceCk)
	if w.Code != http.StatusOK {
		t.Fatalf("member list: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			List []struct {
				ID     uint `json:"id"`
				UserID uint `json:"user_id"`
			} `json:"list"`
			Total int64 `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.Total != 1 || len(env.Data.List) != 1 || env.Data.List[0].ID != f.aliceKeyID {
		t.Fatalf("member list must contain exactly alice's key, got %+v", env.Data)
	}

	// By-id reach into bob's key must be byte-indistinguishable from a key
	// that never existed (the project's not-found shape is 400 + code
	// 11001) — get / plaintext / patch / revoke alike.
	ghost := f.do(t, http.MethodGet, "/api/admin/api-keys/999999", "", f.aliceCk)
	bobKey := func(suffix string) string {
		return "/api/admin/api-keys/" + uintToString(f.bobKeyID) + suffix
	}
	for _, probe := range []struct{ method, path, body string }{
		{http.MethodGet, bobKey(""), ""},
		{http.MethodGet, bobKey("/plaintext"), ""},
		{http.MethodPatch, bobKey(""), `{"remark":"mine now"}`},
		{http.MethodPatch, bobKey("/revoke"), ""},
	} {
		w := f.do(t, probe.method, probe.path, probe.body, f.aliceCk)
		if w.Code != ghost.Code {
			t.Fatalf("%s %s: expected the not-found status %d for another member's key, got %d %s", probe.method, probe.path, ghost.Code, w.Code, w.Body.String())
		}
		var got, want struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal probe body: %v", err)
		}
		if err := json.Unmarshal(ghost.Body.Bytes(), &want); err != nil {
			t.Fatalf("unmarshal ghost body: %v", err)
		}
		if got.Code != want.Code {
			t.Fatalf("%s %s: cross-account probe (code %d) is distinguishable from a nonexistent key (code %d)", probe.method, probe.path, got.Code, want.Code)
		}
	}
	// Sanity: bob's key is untouched.
	var stored model.APIKey
	if err := f.db.First(&stored, f.bobKeyID).Error; err != nil {
		t.Fatalf("load bob key: %v", err)
	}
	if stored.Status != model.APIKeyStatusActive || stored.Remark == "mine now" {
		t.Fatalf("bob's key was mutated through a cross-account probe: %+v", stored)
	}
}

// TestMemberKeyCreationFieldBoundary: restricted fields are rejected
// outright; a permitted create lands owned by the member with the
// all-models scope forced on.
func TestMemberKeyCreationFieldBoundary(t *testing.T) {
	f := newMemberScopeFixture(t)

	for _, body := range []string{
		`{"remark":"x","rpm_limit":10}`,
		`{"remark":"x","budget_limit_micros":1000}`,
		`{"remark":"x","model_ids":[1]}`,
		`{"remark":"x","custom_system_prompt_enabled_override":true,"custom_system_prompt_enabled":true,"custom_system_prompt":"p"}`,
		`{"remark":"x","compress_enabled_override":true,"compress_enabled":true}`,
	} {
		w := f.do(t, http.MethodPost, "/api/admin/api-keys", body, f.aliceCk)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("restricted create %s: expected 400, got %d %s", body, w.Code, w.Body.String())
		}
	}

	w := f.do(t, http.MethodPost, "/api/admin/api-keys", `{"remark":"ok"}`, f.aliceCk)
	if w.Code != http.StatusOK {
		t.Fatalf("permitted member create: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			APIKey struct {
				UserID         uint `json:"user_id"`
				AllowAllModels bool `json:"allow_all_models"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !env.Data.APIKey.AllowAllModels {
		t.Fatalf("member-created key must carry the all-models scope")
	}

	// A member PATCH with a restricted field is rejected too.
	w = f.do(t, http.MethodPatch, "/api/admin/api-keys/"+uintToString(f.aliceKeyID), `{"rpm_limit":5}`, f.aliceCk)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("restricted member patch: expected 400, got %d", w.Code)
	}
	// The permitted rename works on their own key.
	w = f.do(t, http.MethodPatch, "/api/admin/api-keys/"+uintToString(f.aliceKeyID), `{"remark":"renamed"}`, f.aliceCk)
	if w.Code != http.StatusOK {
		t.Fatalf("permitted member patch: %d %s", w.Code, w.Body.String())
	}
}

// TestMemberAnalyticsPinnedToOwnRows: the overview total must reflect
// only the member's own traffic even when the query asks for someone
// else, and the provider dimension answers 403.
func TestMemberAnalyticsPinnedToOwnRows(t *testing.T) {
	f := newMemberScopeFixture(t)

	// Alice asks for "everything" (no user filter): totals must still be
	// hers alone (2 calls, 100 micros — not bob's 900).
	w := f.do(t, http.MethodGet, "/api/admin/analytics/overview", "", f.aliceCk)
	if w.Code != http.StatusOK {
		t.Fatalf("member overview: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			TotalCalls int64 `json:"total_calls"`
			CostMicros int64 `json:"cost_micros"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.TotalCalls != 2 || env.Data.CostMicros != 100 {
		t.Fatalf("member overview leaked other accounts' rows: %+v", env.Data)
	}

	// Smuggled query params must not narrow or widen a member's scope:
	// user_id (identity), provider_id (upstream topology — alice's rows
	// are provider 5, so a leaked provider_id=7 filter would zero her
	// totals), and with_failovers (admin-only scan) are each stripped.
	for _, smuggle := range []string{"user_id=0", "provider_id=7", "with_failovers=1"} {
		w = f.do(t, http.MethodGet, "/api/admin/analytics/overview?"+smuggle, "", f.aliceCk)
		if w.Code != http.StatusOK {
			t.Fatalf("member overview with smuggled %s: %d", smuggle, w.Code)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if env.Data.TotalCalls != 2 {
			t.Fatalf("smuggled %s changed a member's scope: %+v", smuggle, env.Data)
		}
	}

	// The provider dimension is operator information.
	w = f.do(t, http.MethodGet, "/api/admin/analytics/report?dimension=provider", "", f.aliceCk)
	if w.Code != http.StatusForbidden {
		t.Fatalf("provider dimension for member: expected 403, got %d", w.Code)
	}
	// Admins keep it.
	w = f.do(t, http.MethodGet, "/api/admin/analytics/report?dimension=provider", "", f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("provider dimension for admin: expected 200, got %d", w.Code)
	}
}

// TestAnalyticsPermissionGroupSplit pins which permission group each
// analytics route sits on: overview/report/export admit a member session
// (the scoped group), compress-stats is operator-only (the protected
// group), and none of them admit an anonymous caller. The four routes look
// alike but sit on two permission groups, and nothing else pins the split —
// the endpoint tests all authenticate as an admin, which both groups admit.
func TestAnalyticsPermissionGroupSplit(t *testing.T) {
	f := newMemberScopeFixture(t)

	for _, path := range []string{
		"/api/admin/analytics/overview",
		"/api/admin/analytics/report?dimension=model",
		"/api/admin/analytics/export?dimension=model",
	} {
		if w := f.do(t, http.MethodGet, path, "", f.aliceCk); w.Code != http.StatusOK {
			t.Fatalf("member GET %s: %d, want 200 (member-visible group)", path, w.Code)
		}
	}
	if w := f.do(t, http.MethodGet, "/api/admin/analytics/compress-stats", "", f.aliceCk); w.Code != http.StatusForbidden {
		t.Fatalf("member GET compress-stats: %d, want 403 (operator-only group)", w.Code)
	}
	if w := f.do(t, http.MethodGet, "/api/admin/analytics/compress-stats", "", f.adminCk); w.Code != http.StatusOK {
		t.Fatalf("admin GET compress-stats: %d, want 200", w.Code)
	}
	if w := f.do(t, http.MethodGet, "/api/admin/analytics/overview", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET overview: %d, want 401", w.Code)
	}
}

// TestMemberDashboardOmitsDeploymentSections: the traffic cards are
// scoped to the member; the upstream-health and setup-funnel sections
// come back zeroed.
func TestMemberDashboardOmitsDeploymentSections(t *testing.T) {
	f := newMemberScopeFixture(t)

	w := f.do(t, http.MethodGet, "/api/admin/dashboard", "", f.aliceCk)
	if w.Code != http.StatusOK {
		t.Fatalf("member dashboard: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Today struct {
				Calls int64 `json:"calls"`
			} `json:"today"`
			RecentFailures []struct {
				RequestID  string `json:"request_id"`
				ProviderID *uint  `json:"provider_id"`
			} `json:"recent_failures"`
			Setup struct {
				Providers     int64 `json:"providers"`
				TotalRequests int64 `json:"total_requests"`
			} `json:"setup"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.Today.Calls != 2 {
		t.Fatalf("member dashboard must count only their own traffic, got %+v", env.Data.Today)
	}
	if env.Data.Setup.TotalRequests != 0 || env.Data.Setup.Providers != 0 {
		t.Fatalf("member dashboard leaked deployment sections: %+v", env.Data.Setup)
	}
	// The recent-failures list keeps the member's own rows but drops the
	// provider identity — which upstream served a request is operator
	// information. Asserted on the raw JSON: the requirement is that the
	// key does not appear at all, and unmarshalling into a pointer could
	// not tell "absent" from "null".
	if len(env.Data.RecentFailures) == 0 {
		t.Fatalf("member dashboard should list the member's own failure")
	}
	if strings.Contains(w.Body.String(), `"provider_id"`) {
		t.Fatalf("member dashboard body carries a provider_id key: %s", w.Body.String())
	}

	// An admin filtering the same dashboard by ?user_id keeps everything:
	// the deployment sections stay populated and the failures keep their
	// provider identity — scoping by account is not the same as being a
	// member.
	w = f.do(t, http.MethodGet, "/api/admin/dashboard?user_id="+uintToString(f.aliceID), "", f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("admin dashboard with user filter: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.Setup.TotalRequests == 0 {
		t.Fatalf("admin user-filtered dashboard lost the deployment sections: %+v", env.Data.Setup)
	}
	if len(env.Data.RecentFailures) == 0 || env.Data.RecentFailures[0].ProviderID == nil {
		t.Fatalf("admin user-filtered dashboard must keep failure provider identities, got %+v", env.Data.RecentFailures)
	}
}

func uintToString(v uint) string {
	return strconv.FormatUint(uint64(v), 10)
}
