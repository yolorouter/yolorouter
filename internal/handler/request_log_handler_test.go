// Package handler tests for M6.1 request-log list + detail endpoints. Uses
// a real RequestLogService over a migrated SQLite DB — the logic worth
// verifying is the SQL → service (JOIN + status derivation + attempts JSON
// parse) → handler → envelope chain, not boilerplate mock interactions, so
// the tests run end-to-end the same way dashboard_handler_test.go does.
package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/internal/service"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
)

// newRequestLogTestRouter wires up the M6.1/M6.2 request-log routes over a
// fresh migrated SQLite DB, using t.TempDir() as the bodies directory. Uses
// a real RequestLogService — see the package doc comment for why.
func newRequestLogTestRouter(t *testing.T) (*gin.Engine, *gorm.DB, *service.RequestLogService) {
	t.Helper()
	r, db, svc, _ := newRequestLogTestRouterWithBodiesDir(t)
	return r, db, svc
}

// newRequestLogTestRouterWithBodiesDir is newRequestLogTestRouter plus the
// bodiesDir it wired GetRequestLogBodyStream to — the stream-body tests
// need it to plant a real file on disk at the path a body row references.
func newRequestLogTestRouterWithBodiesDir(t *testing.T) (*gin.Engine, *gorm.DB, *service.RequestLogService, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	svc := service.NewRequestLogService(db)
	bodiesDir := t.TempDir()
	r := gin.New()
	admin := r.Group("/api/admin")
	// Export is registered BEFORE :requestId so the literal /export path
	// matches first — gin's route tree would otherwise treat "export" as a
	// requestId value.
	admin.GET("/request-logs", GetRequestLogs(svc))
	admin.GET("/request-logs/export", ExportRequestLogsCSV(svc))
	admin.GET("/request-logs/:requestId", GetRequestLogDetail(svc))
	admin.GET("/request-logs/:requestId/body/stream", GetRequestLogBodyStream(svc, bodiesDir))
	return r, db, svc, bodiesDir
}

// seedAPIKey inserts a minimal api_keys row with a unique key_hash and
// returns its assigned ID. request_logs.api_key_id references this row so
// the list/detail handler can be tested with a populated owner_label.
func seedAPIKey(t *testing.T, db *gorm.DB, label string) uint {
	t.Helper()
	now := time.Now().UTC()
	ak := model.APIKey{
		KeyHash:    "test-hash-" + label + "-" + now.Format("150405.000000000"),
		KeyPrefix:  "sk-xx-",
		OwnerLabel: label,
		Status:     model.APIKeyStatusActive,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := db.Create(&ak).Error; err != nil {
		t.Fatalf("seed api_key %q: %v", label, err)
	}
	return ak.ID
}

// seedProvider inserts a providers row and returns its ID, so provider_id
// on request_logs can JOIN to a real provider_name in list/detail tests.
func seedProvider(t *testing.T, db *gorm.DB, name string) uint {
	t.Helper()
	p := model.Provider{
		Name: name, ProviderType: "openai", BaseURL: "https://example.com/v1",
		ManagementStatus: model.ProviderStatusEnabled,
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("seed provider %q: %v", name, err)
	}
	return p.ID
}

// seedRequestLog inserts one request_logs row with the given shape. The
// mutator applies test-specific overrides (status, fail_reason, attempts,
// attempts_detail, FKs) after the sensible defaults are filled in.
func seedRequestLog(t *testing.T, db *gorm.DB, requestID string, ts time.Time, mut func(*model.RequestLog)) {
	t.Helper()
	row := model.RequestLog{
		RequestID:    requestID,
		ModelName:    "gpt-4o-mini",
		IsStream:     false,
		StatusCode:   200,
		InputTokens:  10,
		OutputTokens: 20,
		CostMicros:   100,
		CostKnown:    true,
		Attempts:     1,
		DurationMs:   42,
		CreatedAt:    ts.UTC(),
	}
	if mut != nil {
		mut(&row)
	}
	if err := repository.CreateRequestLog(db, &row); err != nil {
		t.Fatalf("seedRequestLog %q: %v", requestID, err)
	}
}

// listItem mirrors RequestLogListItem's JSON shape — kept as a local
// anonymous struct so the test doesn't import the service-layer DTO type
// directly, same convention as dashboard_handler_test.go.
type listItem struct {
	RequestID    string  `json:"request_id"`
	OwnerLabel   string  `json:"owner_label"`
	ModelName    string  `json:"model_name"`
	ProviderName string  `json:"provider_name"`
	StatusCode   int     `json:"status_code"`
	StatusClass  string  `json:"status_class"`
	IsStream     bool    `json:"is_stream"`
	Attempts     int     `json:"attempts"`
	DurationMs   int64   `json:"duration_ms"`
	FailReason   *string `json:"fail_reason"`
}

func TestListRequestLogsReturnsEmptyByDefault(t *testing.T) {
	r, _, _ := newRequestLogTestRouter(t)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var page struct {
		Total    int64           `json:"total"`
		List     json.RawMessage `json:"list"`
		Page     int             `json:"page"`
		PageSize int             `json:"page_size"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("unmarshal page: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("expected total=0, got %d", page.Total)
	}
	if string(page.List) != "[]" {
		t.Fatalf("expected empty list, got %s", page.List)
	}
	if page.Page != 1 || page.PageSize != 20 {
		t.Fatalf("expected default page=1/size=20, got %d/%d", page.Page, page.PageSize)
	}
}

func TestListRequestLogsJoinsOwnerLabelAndProviderName(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	keyID := seedAPIKey(t, db, "alice")
	provID := seedProvider(t, db, "openai-main")
	now := time.Now().UTC()
	seedRequestLog(t, db, "req-join-1", now, func(r *model.RequestLog) {
		r.APIKeyID = &keyID
		r.ProviderID = &provID
	})

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var page struct {
		List []listItem `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("unmarshal page: %v", err)
	}
	if len(page.List) != 1 {
		t.Fatalf("expected 1 row, got %d", len(page.List))
	}
	row := page.List[0]
	if row.OwnerLabel != "alice" {
		t.Errorf("owner_label: want alice, got %q", row.OwnerLabel)
	}
	if row.ProviderName != "openai-main" {
		t.Errorf("provider_name: want openai-main, got %q", row.ProviderName)
	}
	if row.StatusClass != "success" {
		t.Errorf("status_class: want success, got %q", row.StatusClass)
	}
}

// TestListRequestLogsDerivesStatusClassForEachBucket walks every status
// bucket (success / partial / cancelled / rejected / failed) and asserts
// service.DeriveStatusClass maps it correctly in the list response. The
// same derivation powers the detail endpoint, so this is the canonical
// test for that logic.
func TestListRequestLogsDerivesStatusClassForEachBucket(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	now := time.Now().UTC()
	reason := "stream truncated"
	cases := []struct {
		requestID  string
		statusCode int
		failReason *string
		want       string
	}{
		{"req-success", 200, nil, "success"},
		{"req-partial", 200, &reason, "partial"},
		{"req-cancel", 499, nil, "cancelled"},
		{"req-reject-401", 401, nil, "rejected"},
		{"req-reject-403", 403, nil, "rejected"},
		{"req-reject-429", 429, nil, "rejected"},
		{"req-fail-500", 500, nil, "failed"},
		{"req-fail-400", 400, nil, "failed"},
	}
	for _, tc := range cases {
		seedRequestLog(t, db, tc.requestID, now, func(r *model.RequestLog) {
			r.StatusCode = tc.statusCode
			r.FailReason = tc.failReason
		})
	}

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs?page_size=50", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var page struct {
		List []listItem `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byReq := make(map[string]string, len(page.List))
	for _, row := range page.List {
		byReq[row.RequestID] = row.StatusClass
	}
	for _, tc := range cases {
		if got := byReq[tc.requestID]; got != tc.want {
			t.Errorf("%s: status_code=%d fail_reason=%v: want %q, got %q",
				tc.requestID, tc.statusCode, tc.failReason, tc.want, got)
		}
	}
}

func TestListRequestLogsPaginatesByCreatedAtDesc(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seedRequestLog(t, db, "req-page-"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Minute), nil)
	}

	// page_size=2 → first page returns the two newest rows (req-page-e, d).
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs?page=1&page_size=2", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var page struct {
		Total int64      `json:"total"`
		List  []listItem `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if page.Total != 5 {
		t.Fatalf("expected total=5, got %d", page.Total)
	}
	if len(page.List) != 2 {
		t.Fatalf("expected 2 rows on page 1, got %d", len(page.List))
	}
	if page.List[0].RequestID != "req-page-e" || page.List[1].RequestID != "req-page-d" {
		t.Fatalf("expected newest two rows first, got %s then %s",
			page.List[0].RequestID, page.List[1].RequestID)
	}
}

func TestListRequestLogFiltersByRequestID(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	now := time.Now().UTC()
	seedRequestLog(t, db, "req-needle-1", now, nil)
	seedRequestLog(t, db, "req-needle-2", now, nil)
	seedRequestLog(t, db, "req-other", now, nil)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs?request_id=req-needle-1", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var page struct {
		Total int64      `json:"total"`
		List  []listItem `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if page.Total != 1 || len(page.List) != 1 {
		t.Fatalf("expected total=1/list=1, got total=%d/list=%d", page.Total, len(page.List))
	}
	if page.List[0].RequestID != "req-needle-1" {
		t.Fatalf("expected req-needle-1, got %q", page.List[0].RequestID)
	}
}

func TestListRequestLogsFiltersByStatusClass(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	now := time.Now().UTC()
	seedRequestLog(t, db, "req-ok", now, func(r *model.RequestLog) { r.StatusCode = 200 })
	seedRequestLog(t, db, "req-err", now, func(r *model.RequestLog) { r.StatusCode = 500 })

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs?status=failed", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var page struct {
		List []listItem `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.List) != 1 || page.List[0].RequestID != "req-err" {
		t.Fatalf("status=failed should return only req-err, got %+v", page.List)
	}
}

func TestListRequestLogsRejectsInvalidStatusClass(t *testing.T) {
	r, _, _ := newRequestLogTestRouter(t)
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs?status=bogus", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.InvalidParam {
		t.Fatalf("expected code %d, got %d", errcode.InvalidParam, env.Code)
	}
}

func TestListRequestLogsFiltersByAPIKeyIDAndProviderID(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	keyA := seedAPIKey(t, db, "alice")
	keyB := seedAPIKey(t, db, "bob")
	provA := seedProvider(t, db, "openai")
	provB := seedProvider(t, db, "anthropic")
	now := time.Now().UTC()
	seedRequestLog(t, db, "req-A", now, func(r *model.RequestLog) {
		r.APIKeyID = &keyA
		r.ProviderID = &provA
	})
	seedRequestLog(t, db, "req-B", now, func(r *model.RequestLog) {
		r.APIKeyID = &keyB
		r.ProviderID = &provB
	})

	w, env := doJSON(t, r, http.MethodGet,
		"/api/admin/request-logs?api_key_id="+uintToStr(keyA)+"&provider_id="+uintToStr(provA), nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var page struct {
		List []listItem `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.List) != 1 || page.List[0].RequestID != "req-A" {
		t.Fatalf("expected only req-A, got %+v", page.List)
	}
	if page.List[0].OwnerLabel != "alice" || page.List[0].ProviderName != "openai" {
		t.Fatalf("expected alice/openai JOIN, got %s/%s",
			page.List[0].OwnerLabel, page.List[0].ProviderName)
	}
}

func TestListRequestLogsFiltersByIsStream(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	now := time.Now().UTC()
	seedRequestLog(t, db, "req-stream", now, func(r *model.RequestLog) { r.IsStream = true })
	seedRequestLog(t, db, "req-nonstream", now, func(r *model.RequestLog) { r.IsStream = false })

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs?is_stream=true", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var page struct {
		List []listItem `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.List) != 1 || page.List[0].RequestID != "req-stream" {
		t.Fatalf("is_stream=true should return only req-stream, got %+v", page.List)
	}
}

func TestListRequestLogsFiltersByTimeRange(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	t0 := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	seedRequestLog(t, db, "req-before", t0.Add(-1*time.Hour), nil)
	seedRequestLog(t, db, "req-inside", t0.Add(1*time.Hour), nil)
	seedRequestLog(t, db, "req-after", t0.Add(3*time.Hour), nil)

	start := t0.Format(time.RFC3339)
	end := t0.Add(2 * time.Hour).Format(time.RFC3339)
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs?start="+start+"&end="+end, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var page struct {
		List []listItem `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.List) != 1 || page.List[0].RequestID != "req-inside" {
		t.Fatalf("start/end window should return only req-inside, got %+v", page.List)
	}
}

// TestListRequestLogsRejectsMalformedQueryParams exercises every optional
// query-param parser's 400 branch. Each case is one parser's failure path
// in isolation, so the failure mapping stays unambiguous.
func TestListRequestLogsRejectsMalformedQueryParams(t *testing.T) {
	r, _, _ := newRequestLogTestRouter(t)
	cases := []struct {
		name, query string
	}{
		{"BadAPIKeyID", "api_key_id=abc"},
		{"BadProviderID", "provider_id=-1"},
		{"BadIsStream", "is_stream=yes"},
		{"BadStart", "start=2026-07-20"},
		{"BadEnd", "end=not-a-time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs?"+tc.query, nil, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
			}
			if env.Code != errcode.InvalidParam {
				t.Fatalf("expected code %d, got %d", errcode.InvalidParam, env.Code)
			}
		})
	}
}

func TestListRequestLogsReturns500WhenDBErrors(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	seedRequestLog(t, db, "req-then-db-closed", time.Now().UTC(), nil)
	testutil.CloseDB(t, db)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs", nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.InternalError {
		t.Fatalf("expected code %d, got %d", errcode.InternalError, env.Code)
	}
}

func TestGetRequestLogDetailReturnsParsedAttempts(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	keyID := seedAPIKey(t, db, "alice")
	provID := seedProvider(t, db, "openai")
	now := time.Now().UTC()
	// Two-attempt failover: first attempt 429 (rotate key), second 200 OK.
	detail := `[` +
		`{"candidate_id":1,"provider_id":` + uintToStr(provID) + `,"provider_name":"openai","provider_model_name":"gpt-4o-mini","key_id":` + uintToStr(keyID) + `,"key_label":"primary","status_code":429,"outcome":"rate_limited","fail_reason":"upstream rate limited"},` +
		`{"candidate_id":1,"provider_id":` + uintToStr(provID) + `,"provider_name":"openai","provider_model_name":"gpt-4o-mini","key_id":` + uintToStr(keyID) + `,"key_label":"secondary","status_code":200,"outcome":"success","fail_reason":""}` +
		`]`
	seedRequestLog(t, db, "req-failover", now, func(r *model.RequestLog) {
		r.APIKeyID = &keyID
		r.ProviderID = &provID
		r.Attempts = 2
		r.AttemptsDetail = &detail
	})

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs/req-failover", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var d struct {
		RequestID      string `json:"request_id"`
		OwnerLabel     string `json:"owner_label"`
		ProviderName   string `json:"provider_name"`
		StatusCode     int    `json:"status_code"`
		StatusClass    string `json:"status_class"`
		Attempts       int    `json:"attempts"`
		AttemptsDetail []struct {
			KeyLabel   string `json:"key_label"`
			StatusCode int    `json:"status_code"`
			Outcome    string `json:"outcome"`
		} `json:"attempts_detail"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.OwnerLabel != "alice" || d.ProviderName != "openai" {
		t.Errorf("JOIN: want alice/openai, got %q/%q", d.OwnerLabel, d.ProviderName)
	}
	if len(d.AttemptsDetail) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(d.AttemptsDetail))
	}
	if d.AttemptsDetail[0].KeyLabel != "primary" || d.AttemptsDetail[0].Outcome != "rate_limited" {
		t.Errorf("attempt[0]: want primary/rate_limited, got %s/%s",
			d.AttemptsDetail[0].KeyLabel, d.AttemptsDetail[0].Outcome)
	}
	if d.AttemptsDetail[1].KeyLabel != "secondary" || d.AttemptsDetail[1].Outcome != "success" {
		t.Errorf("attempt[1]: want secondary/success, got %s/%s",
			d.AttemptsDetail[1].KeyLabel, d.AttemptsDetail[1].Outcome)
	}
}

// TestGetRequestLogDetailReturnsEmptyAttemptsWhenAbsent covers the
// "pre-check failure before any candidate was tried" case (gateway/log.go):
// attempts_detail is NULL, attempts is 0. The detail response must return
// an empty (non-null) array so the frontend doesn't need a nil-check.
func TestGetRequestLogDetailReturnsEmptyAttemptsWhenAbsent(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	seedRequestLog(t, db, "req-precheck-fail", time.Now().UTC(), func(r *model.RequestLog) {
		r.StatusCode = 401
		r.Attempts = 0
	})

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs/req-precheck-fail", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(env.Data, []byte(`"attempts_detail":[]`)) {
		t.Fatalf("expected attempts_detail to be [], got: %s", env.Data)
	}
}

func TestGetRequestLogDetailReturns404WhenNotFound(t *testing.T) {
	r, _, _ := newRequestLogTestRouter(t)
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs/req-missing", nil, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.RequestLogNotFound {
		t.Fatalf("expected code %d, got %d", errcode.RequestLogNotFound, env.Code)
	}
}

func TestGetRequestLogDetailReturns500WhenDBErrors(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	seedRequestLog(t, db, "req-then-db-closed", time.Now().UTC(), nil)
	testutil.CloseDB(t, db)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs/req-then-db-closed", nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.InternalError {
		t.Fatalf("expected code %d, got %d", errcode.InternalError, env.Code)
	}
}

// TestGetRequestLogBodyStreamReturnsFile plants a real file under the
// test's bodiesDir and a request_log_bodies row referencing it, then
// asserts GET .../body/stream serves the file's exact bytes with a
// text/plain Content-Type.
func TestGetRequestLogBodyStreamReturnsFile(t *testing.T) {
	r, db, _, bodiesDir := newRequestLogTestRouterWithBodiesDir(t)
	seedRequestLog(t, db, "req-stream-body", time.Now().UTC(), func(r *model.RequestLog) {
		r.IsStream = true
	})
	const streamContent = "data: {\"chunk\":1}\n\ndata: [DONE]\n\n"
	if err := os.WriteFile(filepath.Join(bodiesDir, "req-stream-body.stream"), []byte(streamContent), 0o600); err != nil {
		t.Fatalf("write stream file: %v", err)
	}
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:      "req-stream-body",
		StreamBodyPath: "req-stream-body.stream",
	}); err != nil {
		t.Fatalf("upsert body row: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/request-logs/req-stream-body/body/stream", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != streamContent {
		t.Fatalf("expected body %q, got %q", streamContent, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("expected text/plain Content-Type, got %q", ct)
	}
}

// TestGetRequestLogBodyStreamNotFound covers both 404 shapes: a
// non-streaming request (no body row at all, so stream_body_path is
// effectively empty) and a body row whose stream_body_path points at a
// file that no longer exists on disk.
func TestGetRequestLogBodyStreamNotFound(t *testing.T) {
	r, db, _, _ := newRequestLogTestRouterWithBodiesDir(t)
	seedRequestLog(t, db, "req-nonstream-body", time.Now().UTC(), func(r *model.RequestLog) {
		r.IsStream = false
	})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/request-logs/req-nonstream-body/body/stream", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a request with no body row, got %d, body: %s", w.Code, w.Body.String())
	}

	seedRequestLog(t, db, "req-missing-stream-file", time.Now().UTC(), func(r *model.RequestLog) {
		r.IsStream = true
	})
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:      "req-missing-stream-file",
		StreamBodyPath: "req-missing-stream-file.stream",
	}); err != nil {
		t.Fatalf("upsert body row: %v", err)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/api/admin/request-logs/req-missing-stream-file/body/stream", nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when stream file is missing on disk, got %d, body: %s", w2.Code, w2.Body.String())
	}
}

func TestExportRequestLogsCSVReturnsBOMAndHeaderAndRows(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	keyID := seedAPIKey(t, db, "alice")
	provID := seedProvider(t, db, "openai")
	now := time.Now().UTC()
	seedRequestLog(t, db, "req-exp-1", now, func(r *model.RequestLog) {
		r.APIKeyID = &keyID
		r.ProviderID = &provID
	})
	seedRequestLog(t, db, "req-exp-2", now, func(r *model.RequestLog) {
		r.APIKeyID = &keyID
		r.ProviderID = &provID
		r.StatusCode = 500
	})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/request-logs/export", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.Bytes()
	// UTF-8 BOM is the first three bytes — Excel/Sheets use it to detect
	// the encoding for CJK owner_label / model_name columns.
	if len(body) < 3 || body[0] != 0xEF || body[1] != 0xBB || body[2] != 0xBF {
		t.Fatalf("expected UTF-8 BOM (EF BB BF) at start, got %x", body[:min(3, len(body))])
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") ||
		!strings.Contains(cd, ".csv") {
		t.Fatalf("expected attachment CSV Content-Disposition, got %q", cd)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("expected text/csv Content-Type, got %q", ct)
	}

	// Strip BOM for parsing the CSV header row.
	text := string(body[3:])
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 CSV lines (header + 2 rows), got %d: %q", len(lines), text)
	}
	if !strings.HasPrefix(lines[0], "request_id,created_at,status_class") {
		t.Fatalf("unexpected CSV header: %q", lines[0])
	}
	// Both data rows should be present; the success row references req-exp-1.
	if !strings.Contains(text, "req-exp-1") || !strings.Contains(text, "req-exp-2") {
		t.Fatalf("expected both request IDs in CSV, got: %s", text)
	}
	// Desensitization: no plaintext key material is ever stored on the
	// row (M5 only kept id/prefix), so the CSV's owner_label column is the
	// admin label "alice" — never a key string. The JOIN'd rows satisfy
	// §6.8.6 by construction; assert the label appears.
	if !strings.Contains(text, "alice") {
		t.Fatalf("expected owner_label 'alice' in CSV, got: %s", text)
	}
}

func TestExportRequestLogsCSVAppliesFilter(t *testing.T) {
	r, db, _ := newRequestLogTestRouter(t)
	now := time.Now().UTC()
	seedRequestLog(t, db, "req-export-ok", now, func(r *model.RequestLog) { r.StatusCode = 200 })
	seedRequestLog(t, db, "req-export-fail", now, func(r *model.RequestLog) { r.StatusCode = 500 })

	req := httptest.NewRequest(http.MethodGet, "/api/admin/request-logs/export?status=failed", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "req-export-fail") {
		t.Errorf("expected failed row in filtered export, got: %s", body)
	}
	if strings.Contains(body, "req-export-ok") {
		t.Errorf("expected success row to be filtered out, got: %s", body)
	}
}

// TestExportRequestLogsCSVRejectsBadStatusBeforeStream ensures the 400 for
// an invalid status class wins over the CSV stream — once the BOM is on
// the wire we can no longer swap to the JSON envelope, so validation must
// happen first.
func TestExportRequestLogsCSVRejectsBadStatusBeforeStream(t *testing.T) {
	r, _, _ := newRequestLogTestRouter(t)
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/request-logs/export?status=bogus", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.InvalidParam {
		t.Fatalf("expected code %d, got %d", errcode.InvalidParam, env.Code)
	}
	if w.Body.Len() > 0 && bytes.HasPrefix(w.Body.Bytes(), []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatalf("BOM should not have been written for a validation failure")
	}
}

// uintToStr formats v as a decimal string. Thin wrapper around
// strconv.FormatUint so test URL builders stay readable.
func uintToStr(v uint) string {
	return strconv.FormatUint(uint64(v), 10)
}
