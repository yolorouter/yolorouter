package handler

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
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

// auditQueryFixture is one query-endpoint test's world: a fully migrated
// temp SQLite database seeded with a fixed grid of audit rows that spans
// every filter dimension, behind a bare gin engine exposing exactly the
// two audit routes. No session layer — these tests pin the query contract
// (filters / intersection / pagination / CSV), not route protection,
// which the router-level conformance sweeps own.
type auditQueryFixture struct {
	r    *gin.Engine
	db   *gorm.DB
	base time.Time
}

// The seeded grid, summarized for the assertions below — six rows whose
// admin, action, resource, status and time dimensions all cross. Times
// hang off one fixed base so the start/end filter edges are exact: the
// custom-system-prompt success (admin 1, base−3h), provider 7 success
// (admin 1, base−2h), provider 8 denied (admin 2, base−90m), the
// input-compression error (admin 2, base−30m), provider 9 success (admin
// 1, base−10m, snapshot username shaped like a spreadsheet formula), and
// a NULL-identity auth.login success (base−4h).
func newAuditQueryFixture(t *testing.T) *auditQueryFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	adminA := testutil.Ptr(uint(1))
	adminB := testutil.Ptr(uint(2))
	rows := []model.AdminAuditLog{
		{CreatedAt: base.Add(-3 * time.Hour), AdminUserID: adminA, AdminUsername: "alice",
			Action: "system_settings.update", ResourceType: "system_setting", ResourceID: "custom-system-prompt",
			Status: model.AuditStatusSuccess},
		{CreatedAt: base.Add(-2 * time.Hour), AdminUserID: adminA, AdminUsername: "alice",
			Action: "provider.create", ResourceType: "provider", ResourceID: "7",
			Status: model.AuditStatusSuccess},
		{CreatedAt: base.Add(-90 * time.Minute), AdminUserID: adminB, AdminUsername: "bob",
			Action: "provider.create", ResourceType: "provider", ResourceID: "8",
			Status: model.AuditStatusDenied},
		{CreatedAt: base.Add(-30 * time.Minute), AdminUserID: adminB, AdminUsername: "bob",
			Action: "system_settings.update", ResourceType: "system_setting", ResourceID: "input-compression",
			Status: model.AuditStatusError, ErrorBrief: "http_status=500"},
		{CreatedAt: base.Add(-10 * time.Minute), AdminUserID: adminA, AdminUsername: "=SUM(A1:A9)",
			Action: "provider.create", ResourceType: "provider", ResourceID: "9",
			Status: model.AuditStatusSuccess},
		{CreatedAt: base.Add(-4 * time.Hour), AdminUserID: nil, AdminUsername: "",
			Action: "auth.login", ResourceType: "auth", ResourceID: "",
			Status: model.AuditStatusSuccess},
	}
	if err := repository.CreateAdminAuditLogs(db, rows); err != nil {
		t.Fatalf("seed audit grid: %v", err)
	}

	r := gin.New()
	r.GET("/api/admin/audit-logs", GetAuditLogs(db))
	r.GET("/api/admin/audit-logs/export", ExportAuditLogsCSV(db))
	return &auditQueryFixture{r: r, db: db, base: base}
}

// list fires GET /api/admin/audit-logs with the given raw query and decodes
// the page envelope. Fails the test on a non-200 so callers assert only
// happy-path shape.
func (f *auditQueryFixture) list(t *testing.T, query string) pageEnvelope {
	t.Helper()
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %s", query, w.Code, w.Body.String())
	}
	return decodePage(t, w.Body.Bytes())
}

// pageEnvelope is the response envelope's data half for paginated lists.
type pageEnvelope struct {
	Total    int64           `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
	List     []auditListItem `json:"list"`
}

// auditListItem is one row as the wire carries it (the model's json tags).
type auditListItem struct {
	ID            uint   `json:"id"`
	AdminUserID   *uint  `json:"admin_user_id"`
	AdminUsername string `json:"admin_username"`
	Action        string `json:"action"`
	ResourceType  string `json:"resource_type"`
	ResourceID    string `json:"resource_id"`
	ErrorBrief    string `json:"error_brief"`
	Status        string `json:"status"`
}

func decodePage(t *testing.T, body []byte) pageEnvelope {
	t.Helper()
	var resp struct {
		Data pageEnvelope `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal page envelope: %v\n%s", err, body)
	}
	return resp.Data
}

// actionsOf folds a page's rows into a comma-joined action string so a
// whole result set compares in one expression.
func actionsOf(rows []auditListItem) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, r.Action)
	}
	return strings.Join(parts, ",")
}

// TestAuditLogsFilterByEachDimension pins per-dimension filtering: every
// filter dimension on its own returns exactly the rows that dimension
// selects — including that an admin filter never matches the NULL-identity
// row and that the three status literals each isolate their own rows.
func TestAuditLogsFilterByEachDimension(t *testing.T) {
	f := newAuditQueryFixture(t)

	// No filter: the whole grid, newest first.
	got := f.list(t, "")
	if got.Total != 6 || len(got.List) != 6 {
		t.Fatalf("unfiltered: total %d len %d, want 6/6", got.Total, len(got.List))
	}
	if want := "provider.create,system_settings.update,provider.create,provider.create,system_settings.update,auth.login"; actionsOf(got.List) != want {
		t.Fatalf("unfiltered order = %s, want %s", actionsOf(got.List), want)
	}

	// Admin: alice's three rows; the NULL-identity row never matches an
	// admin filter.
	if got := f.list(t, "?admin_user_id=1"); got.Total != 3 || actionsOf(got.List) != "provider.create,provider.create,system_settings.update" {
		t.Fatalf("admin 1: total %d actions %s", got.Total, actionsOf(got.List))
	}
	if got := f.list(t, "?admin_user_id=2"); got.Total != 2 || actionsOf(got.List) != "system_settings.update,provider.create" {
		t.Fatalf("admin 2: total %d actions %s", got.Total, actionsOf(got.List))
	}

	// Action.
	if got := f.list(t, "?action=provider.create"); got.Total != 3 {
		t.Fatalf("action provider.create: total %d, want 3", got.Total)
	}
	if got := f.list(t, "?action=auth.login"); got.Total != 1 || got.List[0].AdminUserID != nil {
		t.Fatalf("action auth.login: total %d, want the 1 NULL-identity row", got.Total)
	}

	// Resource: type and id as separate dimensions.
	if got := f.list(t, "?resource_type=provider"); got.Total != 3 {
		t.Fatalf("resource_type provider: total %d, want 3", got.Total)
	}
	if got := f.list(t, "?resource_id=8"); got.Total != 1 || got.List[0].Status != model.AuditStatusDenied {
		t.Fatalf("resource_id 8: total %d, want the 1 denied provider.create", got.Total)
	}

	// Status: the three literals each isolate their own rows.
	if got := f.list(t, "?status=success"); got.Total != 4 {
		t.Fatalf("status success: total %d, want 4", got.Total)
	}
	if got := f.list(t, "?status=denied"); got.Total != 1 || got.List[0].ResourceID != "8" {
		t.Fatalf("status denied: total %d, want the 1 provider 8 row", got.Total)
	}
	if got := f.list(t, "?status=error"); got.Total != 1 || got.List[0].ErrorBrief != "http_status=500" {
		t.Fatalf("status error: total %d, want the 1 input-compression row with its brief", got.Total)
	}

	// Time: start inclusive, end exclusive, edges exact.
	start := f.base.Add(-1 * time.Hour).Format(time.RFC3339)
	if got := f.list(t, "?start="+start); got.Total != 2 || actionsOf(got.List) != "provider.create,system_settings.update" {
		t.Fatalf("start: total %d actions %s, want the 2 rows at/after base−1h", got.Total, actionsOf(got.List))
	}
	if got := f.list(t, "?end="+start); got.Total != 4 {
		t.Fatalf("end: total %d, want the 4 rows strictly before base−1h", got.Total)
	}
}

// TestAuditLogsCombinedFiltersIntersect pins combined filtering:
// multiple present dimensions compose as an intersection, and piling on
// more dimensions only ever shrinks the set.
func TestAuditLogsCombinedFiltersIntersect(t *testing.T) {
	f := newAuditQueryFixture(t)

	// alice ∩ provider.create ∩ success = her two provider rows.
	got := f.list(t, "?admin_user_id=1&action=provider.create&status=success")
	if got.Total != 2 || actionsOf(got.List) != "provider.create,provider.create" {
		t.Fatalf("combined admin/action/status: total %d actions %s, want 2 provider.create", got.Total, actionsOf(got.List))
	}

	// Adding the time dimension shrinks to the one recent row.
	start := f.base.Add(-30 * time.Minute).Format(time.RFC3339)
	got = f.list(t, "?admin_user_id=1&action=provider.create&status=success&start="+start)
	if got.Total != 1 || got.List[0].ResourceID != "9" {
		t.Fatalf("combined with time: total %d, want only the resource 9 row", got.Total)
	}

	// An intersection with nothing in common is a clean empty page, not an
	// error: bob ∩ auth.login matches no row.
	got = f.list(t, "?admin_user_id=2&action=auth.login")
	if got.Total != 0 || len(got.List) != 0 {
		t.Fatalf("empty intersection: total %d len %d, want 0/0", got.Total, len(got.List))
	}
}

// TestAuditLogsPagination pins list pagination: page slicing is
// newest-first, totals stay whole-filter, the last partial page lands, and
// a page past the end is a clean empty list.
func TestAuditLogsPagination(t *testing.T) {
	f := newAuditQueryFixture(t)

	wantPages := [][]string{
		{"provider.create", "system_settings.update"},
		{"provider.create", "provider.create"},
		{"system_settings.update", "auth.login"},
	}
	for i, want := range wantPages {
		page := i + 1
		got := f.list(t, fmt.Sprintf("?page_size=2&page=%d", page))
		if got.Total != 6 {
			t.Fatalf("page %d: total %d, want 6 on every page", page, got.Total)
		}
		if len(got.List) != 2 {
			t.Fatalf("page %d: len %d, want 2", page, len(got.List))
		}
		if a := actionsOf(got.List); a != want[0]+","+want[1] {
			t.Fatalf("page %d actions = %s, want %s,%s", page, a, want[0], want[1])
		}
	}

	// Past the end: empty list, whole-filter total intact.
	if got := f.list(t, "?page_size=2&page=4"); got.Total != 6 || len(got.List) != 0 {
		t.Fatalf("page past end: total %d len %d, want 6/0", got.Total, len(got.List))
	}

	// Defaults: no params = page 1 at the default page size.
	if got := f.list(t, ""); got.Page != 1 || got.PageSize != 20 || len(got.List) != 6 {
		t.Fatalf("defaults: page %d size %d len %d, want 1/20/6", got.Page, got.PageSize, len(got.List))
	}
}

// TestAuditLogsRejectsMalformedFilters pins the 400 forms: an unknown
// status literal, a non-numeric admin id, and a non-RFC3339 timestamp all
// answer the JSON envelope, never a silent filter-off.
func TestAuditLogsRejectsMalformedFilters(t *testing.T) {
	f := newAuditQueryFixture(t)
	for _, q := range []string{
		"?status=weird",
		"?admin_user_id=abc",
		"?start=yesterday",
	} {
		w := httptest.NewRecorder()
		f.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s: status %d, want 400 (body %s)", q, w.Code, w.Body.String())
		}
	}
}

// TestAuditLogsExportCSV pins the CSV export: it carries the BOM
// and the header row, respects the same filters as the list, and — the
// red line — every cell that could start a spreadsheet formula is
// neutralized by the WriteCSV path's per-cell sanitize.
func TestAuditLogsExportCSV(t *testing.T) {
	f := newAuditQueryFixture(t)

	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs/export", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("export: status %d, body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/csv; charset=utf-8", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("content-disposition = %q, want an attachment", cd)
	}

	body := w.Body.Bytes()
	// BOM first — the Excel CJK-compat convention WriteCSV owns.
	if !bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatalf("export does not start with the UTF-8 BOM")
	}
	recs, err := csv.NewReader(bytes.NewReader(body[3:])).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(recs) != 7 { // header + 6 data rows
		t.Fatalf("export holds %d records, want header + 6 rows", len(recs))
	}
	wantHeader := []string{"id", "created_at", "admin_user_id", "admin_username", "action",
		"resource_type", "resource_id", "detail", "status", "error_brief",
		"source_ip", "user_agent", "request_id"}
	for i, col := range wantHeader {
		if recs[0][i] != col {
			t.Fatalf("header col %d = %q, want %q", i, recs[0][i], col)
		}
	}

	// Formula injection: the seeded snapshot username "=SUM(A1:A9)" must
	// arrive tab-prefixed — the OWASP mitigation csvutil.SanitizeCell
	// applies — never as a live formula cell.
	found := false
	for _, rec := range recs[1:] {
		if rec[4] == "provider.create" && rec[6] == "9" {
			found = true
			if rec[3] != "\t=SUM(A1:A9)" {
				t.Fatalf("formula-looking username cell = %q, want tab-prefixed %q", rec[3], "\t=SUM(A1:A9)")
			}
		}
		if rec[3] != "" && rec[3][0] == '=' {
			t.Fatalf("cell %q begins with '=' — the sanitize guard did not run", rec[3])
		}
	}
	if !found {
		t.Fatalf("the resource-9 row never appeared in the export")
	}

	// The export respects the list's filters: same query bytes, smaller set.
	w = httptest.NewRecorder()
	f.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs/export?action=provider.create", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("filtered export: status %d", w.Code)
	}
	recs, err = csv.NewReader(bytes.NewReader(w.Body.Bytes()[3:])).ReadAll()
	if err != nil {
		t.Fatalf("parse filtered csv: %v", err)
	}
	if len(recs) != 4 { // header + 3 provider.create rows
		t.Fatalf("filtered export holds %d records, want header + 3 rows", len(recs))
	}

	// Malformed filters keep the JSON 400 envelope on the export too.
	w = httptest.NewRecorder()
	f.r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs/export?status=weird", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("export with bad status: %d, want 400", w.Code)
	}
}

// TestAuditLogsUsernameSnapshotSurvivesRenameAndDeletion pins the
// snapshot semantics: the query endpoint returns the capture-time
// username snapshot, so renaming the admin (even by direct SQL — the
// harshest rename) and deleting another account outright leave history
// reading exactly as it was written, ids included. Own fixture, not the
// filter grid: the accounts must be the only rows in the table so the
// admin filter totals pin to 1.
func TestAuditLogsUsernameSnapshotSurvivesRenameAndDeletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	r := gin.New()
	r.GET("/api/admin/audit-logs", GetAuditLogs(db))

	now := time.Now().UTC()
	boss := &model.User{Username: "renameme", Role: model.RoleAdmin, Status: model.UserStatusEnabled,
		IsLocal: true, IsBootstrap: true, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(db, boss); err != nil {
		t.Fatalf("seed boss: %v", err)
	}
	doomed := &model.User{Username: "doomed", Role: model.RoleMember, Status: model.UserStatusEnabled,
		IsLocal: true, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(db, doomed); err != nil {
		t.Fatalf("seed doomed: %v", err)
	}
	if err := repository.CreateAdminAuditLogs(db, []model.AdminAuditLog{{
		CreatedAt: now.Add(-time.Hour), AdminUserID: &boss.ID, AdminUsername: "renameme",
		Action: "user.update", ResourceType: "user", ResourceID: "5", Status: model.AuditStatusSuccess,
	}, {
		CreatedAt: now.Add(-2 * time.Hour), AdminUserID: &doomed.ID, AdminUsername: "doomed",
		Action: "user.status", ResourceType: "user", ResourceID: "6", Status: model.AuditStatusSuccess,
	}}); err != nil {
		t.Fatalf("seed snapshot rows: %v", err)
	}

	list := func(query string) pageEnvelope {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/audit-logs"+query, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s", query, w.Code, w.Body.String())
		}
		return decodePage(t, w.Body.Bytes())
	}

	// Rename the admin by direct SQL — no repository helper can mangle
	// history more thoroughly than the raw table write.
	if err := db.Table("users").Where("id = ?", boss.ID).Update("username", "renamed-boss").Error; err != nil {
		t.Fatalf("rename boss: %v", err)
	}
	// Delete the other account outright: the row is gone from users.
	if err := db.Where("id = ?", doomed.ID).Delete(&model.User{}).Error; err != nil {
		t.Fatalf("delete doomed: %v", err)
	}

	// The renamed admin's row: old snapshot name, id unchanged.
	got := list("?admin_user_id=" + strconv.FormatUint(uint64(boss.ID), 10))
	if got.Total != 1 || len(got.List) != 1 {
		t.Fatalf("boss rows: total %d, want 1", got.Total)
	}
	row := got.List[0]
	if row.AdminUsername != "renameme" {
		t.Fatalf("snapshot after rename = %q, want the capture-time name %q", row.AdminUsername, "renameme")
	}
	if row.AdminUserID == nil || *row.AdminUserID != boss.ID {
		t.Fatalf("admin_user_id after rename = %v, want unchanged %d", row.AdminUserID, boss.ID)
	}

	// The deleted account's row is still readable, snapshot and id intact.
	got = list("?admin_user_id=" + strconv.FormatUint(uint64(doomed.ID), 10))
	if got.Total != 1 || len(got.List) != 1 {
		t.Fatalf("doomed rows: total %d, want 1 (history outlives the account)", got.Total)
	}
	row = got.List[0]
	if row.AdminUsername != "doomed" || row.AdminUserID == nil || *row.AdminUserID != doomed.ID {
		t.Fatalf("deleted account's row = name %q id %v, want doomed/%d", row.AdminUsername, row.AdminUserID, doomed.ID)
	}
}
