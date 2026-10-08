package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// captureFixture is a minimal engine shaped like the production admin
// group: the request-id middleware, then the audit capture layer at the
// group root, then whatever the test mounts under it. The worker's flush
// interval is an hour, so nothing persists until stop drains — assertions
// about "what got captured" then read a deterministic table instead of
// racing a timer.
type captureFixture struct {
	engine *gin.Engine
	db     *gorm.DB
	group  *gin.RouterGroup
	stop   func()
}

// newCaptureFixture builds the fixture. engineMiddleware runs BEFORE the
// audit layer in the chain — that is where the production engine mounts
// recovery, and the panic test needs that exact nesting to prove the
// re-panic reaches it. (Gin snapshots the handler chain when a group is
// created, so engine-level middleware must be registered here, before the
// group — appending to the engine afterwards would silently miss it.)
func newCaptureFixture(t *testing.T, engineMiddleware ...gin.HandlerFunc) *captureFixture {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	worker := NewWorker(Config{DB: db, FlushInterval: time.Hour})
	stop := worker.Start(context.Background())
	t.Cleanup(stop)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware.RequestID())
	engine.Use(engineMiddleware...)
	group := engine.Group("/api/admin")
	group.Use(Middleware(db, worker))

	return &captureFixture{engine: engine, db: db, group: group, stop: stop}
}

// rows returns the persisted audit rows (call after fixture.stop).
func (f *captureFixture) rows(t *testing.T) []model.AdminAuditLog {
	t.Helper()
	f.stop()
	var rows []model.AdminAuditLog
	if err := f.db.Find(&rows).Error; err != nil {
		t.Fatalf("list audit rows: %v", err)
	}
	return rows
}

func (f *captureFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("User-Agent", "audit-test-agent/1.0")
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	return w
}

// TestUnlistedWriteRouteGetsDerivedAction is the fallback-derivation
// acceptance case: a write route the dictionary has never heard of is
// still audited, with an action derived from its own shape
// (<resource>.<verb>). This is what makes a future endpoint audited on
// day one, before anyone names it.
func TestUnlistedWriteRouteGetsDerivedAction(t *testing.T) {
	f := newCaptureFixture(t)
	f.group.POST("/widgets", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	w := f.do(t, http.MethodPost, "/api/admin/widgets", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("fixture route must answer 200, got %d: %s", w.Code, w.Body.String())
	}
	if KnownAction(http.MethodPost, "/api/admin/widgets") {
		t.Fatal("test premise broken: the temp route must not be in the dictionary")
	}

	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("exactly one audit row must capture the unlisted write, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "widgets.create" {
		t.Errorf("derived action = %q, want widgets.create", row.Action)
	}
	if row.ResourceType != "widgets" {
		t.Errorf("resource type = %q, want widgets", row.ResourceType)
	}
	if row.Status != model.AuditStatusSuccess {
		t.Errorf("status = %q, want success", row.Status)
	}
}

// TestReadsAreNotCaptured pins the noise contract: GET (and HEAD/OPTIONS)
// never produce audit rows from the automatic layer — an audit trail that
// logs every console refresh buries the mutations it exists to record.
// The sensitive-read exceptions are explicit events, not middleware
// captures.
func TestReadsAreNotCaptured(t *testing.T) {
	f := newCaptureFixture(t)
	f.group.GET("/widgets", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	f.group.POST("/widgets", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		f.do(t, method, "/api/admin/widgets", "")
	}
	// Sanity: the write next to them IS captured.
	f.do(t, http.MethodPost, "/api/admin/widgets", `{}`)

	if rows := f.rows(t); len(rows) != 1 {
		t.Fatalf("only the POST may be audited; reads must add nothing, got %d rows", len(rows))
	}
}

// TestEveryWriteMethodCaptured walks the method matrix: POST, PUT, PATCH,
// and DELETE all reach the record.
func TestEveryWriteMethodCaptured(t *testing.T) {
	f := newCaptureFixture(t)
	f.group.Any("/widgets", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		f.do(t, method, "/api/admin/widgets", "")
	}

	rows := f.rows(t)
	if len(rows) != 4 {
		t.Fatalf("all four write methods must be captured, got %d rows", len(rows))
	}
	wantVerbs := map[string]string{
		http.MethodPost:   "widgets.create",
		http.MethodPut:    "widgets.update",
		http.MethodPatch:  "widgets.update",
		http.MethodDelete: "widgets.delete",
	}
	verbs := map[string]string{}
	for _, row := range rows {
		verbs[row.Action] = row.Action
	}
	for _, want := range wantVerbs {
		if verbs[want] == "" {
			t.Errorf("no row carries derived action %q (have %v)", want, verbs)
		}
	}
}

// TestOutcomeClassification pins the tri-state: 2xx is success, 401/403
// is denied (the refusal itself is the signal), everything else is error
// with a brief built from the status code only — never from the response
// body, which may quote request payloads and error internals.
func TestOutcomeClassification(t *testing.T) {
	f := newCaptureFixture(t)
	f.group.POST("/ok", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{}) })
	f.group.POST("/unauthorized", func(c *gin.Context) {
		middleware.WriteAdminError(c, http.StatusUnauthorized, 10001)
	})
	f.group.POST("/forbidden", func(c *gin.Context) {
		middleware.WriteAdminError(c, http.StatusForbidden, 10003)
	})
	f.group.POST("/invalid", func(c *gin.Context) {
		middleware.WriteAdminError(c, http.StatusBadRequest, 10002)
	})
	f.group.POST("/boom", func(c *gin.Context) {
		middleware.WriteAdminError(c, http.StatusInternalServerError, 10500)
	})

	f.do(t, http.MethodPost, "/api/admin/ok", "")
	f.do(t, http.MethodPost, "/api/admin/unauthorized", "")
	f.do(t, http.MethodPost, "/api/admin/forbidden", "")
	f.do(t, http.MethodPost, "/api/admin/invalid", "")
	f.do(t, http.MethodPost, "/api/admin/boom", "")

	got := map[string]string{}
	for _, row := range f.rows(t) {
		got[row.Action] = row.Status + "|" + row.ErrorBrief
	}
	want := map[string]string{
		"ok.create":           "success|",
		"unauthorized.create": "denied|",
		"forbidden.create":    "denied|",
		"invalid.create":      "error|http_status=400",
		"boom.create":         "error|http_status=500",
	}
	for action, wantState := range want {
		if got[action] != wantState {
			t.Errorf("%s: status|brief = %q, want %q", action, got[action], wantState)
		}
	}
}

// TestPanicStillRecordsAndRepanics pins the fire-drill path: a handler
// panic still produces an audit row (classified as error), the panic is
// re-raised so the engine-level recovery middleware still owns the 500
// response, and no database write happens on the request path.
func TestPanicStillRecordsAndRepanics(t *testing.T) {
	// Recovery sits OUTSIDE the audit layer, exactly like the production
	// engine (engine-level middlewares run before group middlewares).
	f := newCaptureFixture(t, middleware.Recovery())
	f.group.POST("/explode", func(c *gin.Context) { panic("kaboom") })

	w := f.do(t, http.MethodPost, "/api/admin/explode", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("recovery middleware must still own the panic response (500), got %d", w.Code)
	}

	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("a panicked write must still be audited, got %d rows", len(rows))
	}
	if rows[0].Status != model.AuditStatusError || rows[0].ErrorBrief != "handler panic" {
		t.Fatalf("panic row = %s|%s, want error|handler panic", rows[0].Status, rows[0].ErrorBrief)
	}
}

// TestIdentitySnapshot pins the actor contract: whatever identity the
// session middleware resolved is captured with its username snapshotted
// from the users table, and an id that no longer resolves still records
// (empty name, intact id) — losing a row over a display name would be the
// wrong trade. Unauthenticated writes keep a NULL admin_user_id, which is
// itself evidence.
func TestIdentitySnapshot(t *testing.T) {
	f := newCaptureFixture(t)

	adminID := testutil.SeedKeyOwner(t, f.db) // an enabled admin account
	ghostID := uint(424242)                   // matches no row

	mount := func(path string, uid *uint) {
		f.group.POST(path, func(c *gin.Context) {
			if uid != nil {
				c.Set(middleware.UserIDKey, *uid)
			}
			c.JSON(http.StatusOK, gin.H{})
		})
	}
	mount("/as-admin", &adminID)
	mount("/as-ghost", &ghostID)
	mount("/anonymous", nil)

	f.do(t, http.MethodPost, "/api/admin/as-admin", "")
	f.do(t, http.MethodPost, "/api/admin/as-ghost", "")
	f.do(t, http.MethodPost, "/api/admin/anonymous", "")

	byAction := map[string]model.AdminAuditLog{}
	for _, row := range f.rows(t) {
		byAction[row.Action] = row
	}

	adminRow, ok := byAction["as_admin.create"]
	if !ok {
		t.Fatalf("no row for the admin-identified request; have %v", byAction)
	}
	if adminRow.AdminUserID == nil || *adminRow.AdminUserID != adminID {
		t.Fatalf("admin row admin_user_id = %v, want %d", adminRow.AdminUserID, adminID)
	}
	// SeedKeyOwner derives "key-owner-<n>"; assert the snapshot is the
	// seeded account's live username by prefix, not by the exact counter.
	if adminRow.AdminUsername == "" || !strings.HasPrefix(adminRow.AdminUsername, "key-owner-") {
		t.Fatalf("admin row username snapshot = %q, want the seeded account's name", adminRow.AdminUsername)
	}

	ghostRow, ok := byAction["as_ghost.create"]
	if !ok {
		t.Fatalf("no row for the deleted-identity request")
	}
	if ghostRow.AdminUserID == nil || *ghostRow.AdminUserID != ghostID || ghostRow.AdminUsername != "" {
		t.Fatalf("unresolvable id must keep its id with an empty snapshot, got id=%v name=%q", ghostRow.AdminUserID, ghostRow.AdminUsername)
	}

	anonRow, ok := byAction["anonymous.create"]
	if !ok {
		t.Fatalf("no row for the anonymous request")
	}
	if anonRow.AdminUserID != nil {
		t.Fatalf("unauthenticated write must keep a NULL admin_user_id, got %v", *anonRow.AdminUserID)
	}
}

// TestRequestIDMatchesResponseHeader pins the reconciliation key at the
// middleware layer: the row's request_id is the same bare UUID the
// request-id middleware echoed as the response's X-Request-Id header.
func TestRequestIDMatchesResponseHeader(t *testing.T) {
	f := newCaptureFixture(t)
	f.group.POST("/widgets", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{}) })

	w := f.do(t, http.MethodPost, "/api/admin/widgets", "")
	header := w.Header().Get("X-Request-Id")
	if header == "" {
		t.Fatal("fixture must echo X-Request-Id like the production engine")
	}

	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].RequestID != header {
		t.Fatalf("row request_id %q != response X-Request-Id %q", rows[0].RequestID, header)
	}
}

// TestDetailSeatFlowsIntoRow pins the merge seat: detail parked on the
// context by the explicit-event layer during the request lands in the one
// row the middleware writes — one request, one row, explicit context
// included. No detail parked, empty column.
func TestDetailSeatFlowsIntoRow(t *testing.T) {
	f := newCaptureFixture(t)
	f.group.POST("/with-detail", func(c *gin.Context) {
		SetDetail(c, `{"changed":["proxy_mode"]}`)
		c.JSON(http.StatusOK, gin.H{})
	})
	f.group.POST("/without-detail", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{}) })

	f.do(t, http.MethodPost, "/api/admin/with-detail", "")
	f.do(t, http.MethodPost, "/api/admin/without-detail", "")

	detail := ""
	for _, row := range f.rows(t) {
		switch row.Action {
		case "with_detail.create":
			detail = row.Detail
		case "without_detail.create":
			if row.Detail != "" {
				t.Errorf("row without explicit detail must store an empty detail, got %q", row.Detail)
			}
		}
	}
	if detail != `{"changed":["proxy_mode"]}` {
		t.Fatalf("detail from the context seat = %q, want the parked JSON verbatim", detail)
	}
}

// TestCapturedRequestContext pins the transport fields: source IP and
// user agent ride the row (the agent capped), and created_at carries the
// capture time rather than the flush time.
func TestCapturedRequestContext(t *testing.T) {
	f := newCaptureFixture(t)
	f.group.POST("/widgets", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{}) })

	before := time.Now().UTC()
	f.do(t, http.MethodPost, "/api/admin/widgets", "")

	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.UserAgent != "audit-test-agent/1.0" {
		t.Errorf("user agent = %q, want the request's header", row.UserAgent)
	}
	if row.SourceIP == "" {
		t.Errorf("source ip must be captured, got empty")
	}
	if row.CreatedAt.Before(before.Add(-time.Second)) || row.CreatedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("created_at %v must reflect the request's capture time", row.CreatedAt)
	}
}
