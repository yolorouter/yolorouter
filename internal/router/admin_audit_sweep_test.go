package router

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/audit"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/auth"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// auditFixture wires the real engine against a started audit worker whose
// flush interval outlives the test: nothing persists until stop drains,
// so "what got captured" is read from a deterministic table. The fixture
// seeds the one bootstrap-style admin account with a live session — the
// same world the member-scope fixture builds, minus the cross-account
// cast.
type auditFixture struct {
	r        *gin.Engine
	db       *gorm.DB
	worker   *audit.Worker
	stop     func()
	adminCk  *http.Cookie
	logoutCk *http.Cookie
	adminID  uint
}

func newAuditFixture(t *testing.T) *auditFixture {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	worker := audit.NewWorker(audit.Config{DB: db, FlushInterval: time.Hour})
	stop := worker.Start(t.Context())
	t.Cleanup(stop)

	deps := testDeps(t, db)
	deps.Audit = worker
	r, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now := time.Now().UTC()
	admin := &model.User{Username: "boss", Role: model.RoleAdmin, Status: model.UserStatusEnabled,
		IsLocal: true, IsBootstrap: true, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(db, admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := repository.CreateSession(db, "tok-audit-admin", admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed admin session: %v", err)
	}
	// The logout endpoint deletes the session its cookie presents; the
	// sweep therefore fires it through a dedicated throwaway session so
	// the main session survives all 56 requests.
	if err := repository.CreateSession(db, "tok-audit-sweep-logout", admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed throwaway logout session: %v", err)
	}

	return &auditFixture{
		r:        r,
		db:       db,
		worker:   worker,
		stop:     stop,
		adminCk:  &http.Cookie{Name: "session_id", Value: "tok-audit-admin"},
		logoutCk: &http.Cookie{Name: "session_id", Value: "tok-audit-sweep-logout"},
		adminID:  admin.ID,
	}
}

// do fires one request with the admin cookie and an empty JSON body, and
// returns the recorder (whose X-Request-Id header reconciles the fired
// request with the audit row it must have produced).
func (f *auditFixture) do(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return f.doWith(t, method, path, f.adminCk)
}

// doWith fires one request presenting a specific session cookie.
func (f *auditFixture) doWith(t *testing.T, method, path string, ck *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

// drain stops the worker (its stop flushes everything still queued, the
// same exit path production uses) and returns the persisted rows.
func (f *auditFixture) drain(t *testing.T) []model.AdminAuditLog {
	t.Helper()
	f.stop()
	var rows []model.AdminAuditLog
	if err := f.db.Find(&rows).Error; err != nil {
		t.Fatalf("list audit rows: %v", err)
	}
	return rows
}

// expectedWriteActions is the route-table-driven expectation for the
// sweep: every console write endpoint that exists, with the action the
// dictionary names it. Duplicated from the dictionary on purpose — the
// sweep then fails if the dictionary drifts from the surface it claims to
// enumerate (same pattern as the member-scope conformance list: an
// explicit table the live router is held against).
var expectedWriteActions = map[string]string{
	// Public auth group.
	"POST /api/admin/auth/setup":       "auth.setup",
	"POST /api/admin/auth/login":       "auth.login",
	"POST /api/admin/auth/oauth/state": "auth.oauth_state",
	// Session-only group.
	"POST /api/admin/auth/logout":  "auth.logout",
	"PUT /api/admin/auth/password": "auth.password_change",
	// Accounts.
	"POST /api/admin/users":              "user.create",
	"POST /api/admin/users/:id/password": "user.password_reset",
	"PATCH /api/admin/users/:id/profile": "user.update",
	"PATCH /api/admin/users/:id/status":  "user.status",
	"PATCH /api/admin/users/:id/role":    "user.role",
	// Caller API keys (ownership-scoped group).
	"POST /api/admin/api-keys":             "api_key.create",
	"PATCH /api/admin/api-keys/:id":        "api_key.update",
	"PATCH /api/admin/api-keys/:id/revoke": "api_key.revoke",
	// Providers and their keys.
	"POST /api/admin/providers":                         "provider.create",
	"POST /api/admin/providers/test-key":                "provider.test_key",
	"POST /api/admin/providers/list-models":             "provider.list_models",
	"PATCH /api/admin/providers/:id":                    "provider.update",
	"PATCH /api/admin/providers/:id/status":             "provider.status",
	"DELETE /api/admin/providers/:id":                   "provider.delete",
	"POST /api/admin/providers/:id/keys":                "provider_key.create",
	"PATCH /api/admin/providers/:id/keys/:keyId":        "provider_key.update",
	"PATCH /api/admin/providers/:id/keys/:keyId/order":  "provider_key.order",
	"PATCH /api/admin/providers/:id/keys/:keyId/status": "provider_key.status",
	"DELETE /api/admin/providers/:id/keys/:keyId":       "provider_key.delete",
	"POST /api/admin/providers/:id/keys/:keyId/test":    "provider_key.test",
	"POST /api/admin/providers/:id/keys/test-all":       "provider_key.test_all",
	"POST /api/admin/providers/:id/keys/:keyId/reveal":  "provider_key.reveal",
	// Learned rate limits.
	"DELETE /api/admin/rate-limits/:id": "rate_limit.delete",
	// Models and candidates.
	"POST /api/admin/models":                                     "model.create",
	"POST /api/admin/models/batch":                               "model.batch_create",
	"PATCH /api/admin/models/:id":                                "model.update",
	"PATCH /api/admin/models/:id/status":                         "model.status",
	"DELETE /api/admin/models/:id":                               "model.delete",
	"POST /api/admin/models/:id/candidates":                      "model_candidate.create",
	"POST /api/admin/models/:id/candidates/test-and-create":      "model_candidate.test_and_create",
	"PATCH /api/admin/models/:id/candidates/:candidateId":        "model_candidate.update",
	"PATCH /api/admin/models/:id/candidates/:candidateId/order":  "model_candidate.order",
	"PATCH /api/admin/models/:id/candidates/:candidateId/status": "model_candidate.status",
	"POST /api/admin/models/:id/candidates/:candidateId/test":    "model_candidate.test",
	"DELETE /api/admin/models/:id/candidates/:candidateId":       "model_candidate.delete",
	// Provider-scoped model-domain operations.
	"POST /api/admin/providers/:id/models/import":         "provider.import_models",
	"POST /api/admin/providers/:id/models/suggest-prices": "provider.suggest_prices",
	// OAuth identity providers.
	"POST /api/admin/oauth-providers":          "oauth_provider.create",
	"PATCH /api/admin/oauth-providers/:id":     "oauth_provider.update",
	"DELETE /api/admin/oauth-providers/:id":    "oauth_provider.delete",
	"POST /api/admin/oauth-providers/discover": "oauth_provider.discover",
	// Playground console runs.
	"POST /api/admin/playground/run":             "playground.run",
	"POST /api/admin/playground/video/stop-wait": "playground.stop_wait",
	// Typed system settings.
	"PUT /api/admin/system-settings/custom-system-prompt":   "system_settings.update",
	"PUT /api/admin/system-settings/input-compression":      "system_settings.update",
	"PUT /api/admin/system-settings/vision-fallback":        "system_settings.update",
	"PUT /api/admin/system-settings/key-auto-recovery":      "system_settings.update",
	"PUT /api/admin/system-settings/request-log-retention":  "system_settings.update",
	"PUT /api/admin/system-settings/admin-audit-retention":  "system_settings.update",
	"PUT /api/admin/system-settings/outbound-proxy":         "system_settings.update",
	"PUT /api/admin/system-settings/capability-enforcement": "system_settings.update",
	// One-click self-update.
	"POST /api/admin/system/update": "system.update",
}

// TestAdminAuditSweepCapturesEveryWriteEndpoint is the capture safety
// net: enumerate the live router's console write routes, fire one request
// at each, and require exactly one audit row per request carrying the
// dictionary's action. A write endpoint the audit layer misses — wrong
// mount point, wrong method filter, a subgroup the root layer somehow
// does not cover — fails here. The set of routes is also held against the
// expectation table, so a NEW console write route cannot appear without a
// conscious dictionary entry here (the fallback derivation still audits
// it at runtime; this test makes the unnamed state visible in review).
func TestAdminAuditSweepCapturesEveryWriteEndpoint(t *testing.T) {
	f := newAuditFixture(t)

	liveWrites := map[string]bool{}
	for _, ri := range f.r.Routes() {
		if !isWriteMethodForSweep(ri.Method) || !strings.HasPrefix(ri.Path, "/api/admin") {
			continue
		}
		liveWrites[ri.Method+" "+ri.Path] = true
	}

	// Route-surface reconciliation: no route missing from the table, no
	// stale table entries.
	for key := range liveWrites {
		if _, ok := expectedWriteActions[key]; !ok {
			t.Errorf("console write route %s is not in this sweep's expectation table — name it in the action dictionary and add the entry here", key)
		}
	}
	for key := range expectedWriteActions {
		if !liveWrites[key] {
			t.Errorf("expectation table names %s, but no such live write route exists — remove the stale entry", key)
		}
	}
	if len(expectedWriteActions) != 57 {
		t.Errorf("expected the console write surface to hold 57 endpoints, table lists %d", len(expectedWriteActions))
	}

	// Fire one request per live write route. Handlers answer with whatever
	// they answer — validation refusals, not-found, conflict are all fine:
	// the audit contract is about capture, not success.
	requestIDs := map[string]string{} // request id -> route key
	for key := range liveWrites {
		method, pattern := splitRouteKey(key)
		concrete := strings.NewReplacer(":id", "1", ":keyId", "1", ":candidateId", "1").Replace(pattern)
		ck := f.adminCk
		if key == "POST /api/admin/auth/logout" {
			// Logout consumes the session it presents; use the throwaway so
			// the main session outlives the sweep.
			ck = f.logoutCk
		}
		w := f.doWith(t, method, concrete, ck)
		reqID := w.Header().Get("X-Request-Id")
		if reqID == "" {
			t.Fatalf("request to %s carried no X-Request-Id — the production request-id middleware must run", key)
		}
		requestIDs[reqID] = key
	}

	rows := f.drain(t)
	if len(rows) != len(liveWrites) {
		t.Fatalf("fired %d write requests, got %d audit rows — every write request must produce exactly one", len(liveWrites), len(rows))
	}
	byRequestID := map[string]model.AdminAuditLog{}
	for _, row := range rows {
		if prev, dup := byRequestID[row.RequestID]; dup {
			t.Fatalf("request %s produced two rows (actions %q and %q)", row.RequestID, prev.Action, row.Action)
		}
		byRequestID[row.RequestID] = row
	}
	for reqID, key := range requestIDs {
		row, ok := byRequestID[reqID]
		if !ok {
			t.Errorf("%s: no audit row for its request", key)
			continue
		}
		if want := expectedWriteActions[key]; row.Action != want {
			t.Errorf("%s: audit action = %q, want dictionary action %q", key, row.Action, want)
		}
		method, pattern := splitRouteKey(key)
		if !audit.KnownAction(method, pattern) {
			t.Errorf("%s: action resolved through the fallback derivation — the dictionary is the enumerated surface, add the entry", key)
		}
		if row.ResourceType == "" {
			t.Errorf("%s: row carries no resource type", key)
		}
		if row.Status == "" {
			t.Errorf("%s: row carries no status", key)
		}
		// The fixture signs every fired request, so every row from a
		// session-gated route must carry the acting admin identity with its
		// username snapshot. The three public auth endpoints are the
		// exception: they run before any session exists, and their rows
		// legitimately carry a NULL identity (the login event's richer
		// encoding is the explicit layer's job, not the capture layer's).
		publicAuth := map[string]bool{
			"POST /api/admin/auth/setup":       true,
			"POST /api/admin/auth/login":       true,
			"POST /api/admin/auth/oauth/state": true,
		}
		if !publicAuth[key] {
			if row.AdminUserID == nil || *row.AdminUserID != f.adminID {
				t.Errorf("%s: row admin_user_id = %v, want the session's admin %d", key, row.AdminUserID, f.adminID)
			}
			if row.AdminUsername != "boss" {
				t.Errorf("%s: row username snapshot = %q, want boss", key, row.AdminUsername)
			}
		}
	}
}

// TestAdminAuditSurfaceHasNoUpdateOrDeleteEndpoint is the immutability
// guarantee at the route surface: audit rows may only ever be read. A
// mutating route anywhere under the audit namespace would be a back door
// around the append-only contract, so this fails on any non-GET audit
// route — including ones registered in the future.
func TestAdminAuditSurfaceHasNoUpdateOrDeleteEndpoint(t *testing.T) {
	f := newAuditFixture(t)
	for _, ri := range f.r.Routes() {
		if !strings.HasPrefix(ri.Path, "/api/admin") {
			continue
		}
		if !strings.Contains(ri.Path, "/audit") {
			continue
		}
		if ri.Method != http.MethodGet {
			t.Errorf("audit route %s %s is a mutation — the audit trail is append-only and exposes reads only", ri.Method, ri.Path)
		}
	}
}

// TestAdminAuditRequestIDReconcilesWithResponseHeader pins the
// reconciliation key end to end through the real engine: the audit row's
// request_id is the response's X-Request-Id header value, a bare UUID
// with no prefix decoration.
func TestAdminAuditRequestIDReconcilesWithResponseHeader(t *testing.T) {
	f := newAuditFixture(t)

	w := f.do(t, http.MethodPut, "/api/admin/system-settings/input-compression")
	header := w.Header().Get("X-Request-Id")
	if header == "" {
		t.Fatal("response carried no X-Request-Id header")
	}
	if _, err := uuid.Parse(header); err != nil {
		t.Fatalf("X-Request-Id %q is not a bare UUID: %v", header, err)
	}

	rows := f.drain(t)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 audit row for the settings PUT, got %d", len(rows))
	}
	row := rows[0]
	if row.RequestID != header {
		t.Fatalf("audit request_id %q != response X-Request-Id %q", row.RequestID, header)
	}
	if _, err := uuid.Parse(row.RequestID); err != nil {
		t.Fatalf("audit request_id %q must be a bare UUID (no prefix): %v", row.RequestID, err)
	}
	if row.Action != "system_settings.update" {
		t.Errorf("action = %q, want system_settings.update", row.Action)
	}
	if row.ResourceID != "input-compression" {
		t.Errorf("resource id = %q, want the setting slug input-compression", row.ResourceID)
	}
}

// TestAdminAuditSessionOnlyEvents pins the session-only subgroup's two
// write events — the endpoints that live OUTSIDE the admin-protected
// group and are exactly the ones a protected-group mount would have
// missed: logout and self password change each land their named action
// with the acting account's identity and snapshot.
func TestAdminAuditSessionOnlyEvents(t *testing.T) {
	f := newAuditFixture(t)

	// A local member account with a real bcrypt password, so the password
	// change runs the full path (verify + rotate + force re-login).
	hash, err := auth.HashPassword("OldPass1234!")
	if err != nil {
		t.Fatalf("hash old password: %v", err)
	}
	now := time.Now().UTC()
	member := &model.User{Username: "carol", Role: model.RoleMember, Status: model.UserStatusEnabled,
		IsLocal: true, PasswordHash: hash, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(f.db, member); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := repository.CreateSession(f.db, "tok-audit-member", member.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed member session: %v", err)
	}
	memberCk := &http.Cookie{Name: "session_id", Value: "tok-audit-member"}

	put := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(memberCk)
		w := httptest.NewRecorder()
		f.r.ServeHTTP(w, req)
		return w
	}
	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.AddCookie(memberCk)
		w := httptest.NewRecorder()
		f.r.ServeHTTP(w, req)
		return w
	}

	w := put("/api/admin/auth/password", `{"current_password":"OldPass1234!","new_password":"NewPass5678!"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("password change must succeed, got %d: %s", w.Code, w.Body.String())
	}
	// Logout after the password change invalidated the old session? No —
	// ChangePassword deletes the session, so sign in fresh state: seed a
	// new session for the logout leg.
	if err := repository.CreateSession(f.db, "tok-audit-member2", member.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed second member session: %v", err)
	}
	memberCk = &http.Cookie{Name: "session_id", Value: "tok-audit-member2"}
	w = post("/api/admin/auth/logout")
	if w.Code != http.StatusOK {
		t.Fatalf("logout must succeed, got %d: %s", w.Code, w.Body.String())
	}

	rows := f.drain(t)
	if len(rows) != 2 {
		t.Fatalf("expected exactly 2 audit rows (password change + logout), got %d", len(rows))
	}
	byAction := map[string]model.AdminAuditLog{}
	for _, row := range rows {
		byAction[row.Action] = row
	}
	pw, ok := byAction["auth.password_change"]
	if !ok {
		t.Fatalf("no auth.password_change row; actions present: %v", byAction)
	}
	if pw.AdminUserID == nil || *pw.AdminUserID != member.ID || pw.AdminUsername != "carol" {
		t.Fatalf("password_change row identity = id:%v name:%q, want member %d carol", pw.AdminUserID, pw.AdminUsername, member.ID)
	}
	if pw.Status != model.AuditStatusSuccess {
		t.Errorf("password_change status = %q, want success", pw.Status)
	}
	lo, ok := byAction["auth.logout"]
	if !ok {
		t.Fatalf("no auth.logout row; actions present: %v", byAction)
	}
	if lo.AdminUserID == nil || *lo.AdminUserID != member.ID || lo.AdminUsername != "carol" {
		t.Fatalf("logout row identity = id:%v name:%q, want member %d carol", lo.AdminUserID, lo.AdminUsername, member.ID)
	}
}

// isWriteMethodForSweep mirrors the middleware's method filter for route
// enumeration.
func isWriteMethodForSweep(method string) bool {
	return slices.Contains([]string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, method)
}

// splitRouteKey splits "METHOD /pattern" back apart.
func splitRouteKey(key string) (method, pattern string) {
	method, pattern, _ = strings.Cut(key, " ")
	if method == "" || pattern == "" {
		panic("bad route key " + key)
	}
	return method, pattern
}
