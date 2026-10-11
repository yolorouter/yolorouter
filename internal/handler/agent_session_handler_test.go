// Handler tests for the tool-session endpoints: the wire shape of the
// aggregate list (page envelope, JSON keys, hand-computed values), the
// agent_client query param, server-side pagination echoes, and the detail
// envelope including the 404 for an unknown session id. Same style as the
// request-log handler tests — a real service over a migrated SQLite DB,
// because the logic worth pinning is the SQL → service → handler → envelope
// chain.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/agentturns"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/requestlog"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// backfillAgentTurns stands in for the startup turns pass over the seeded
// history: the fixtures write the audit rows the gateway wrote, and this
// derives the turn store half exactly as a deployed instance would at boot.
func backfillAgentTurns(t *testing.T, db *gorm.DB) {
	t.Helper()
	if _, err := agentturns.BackfillAll(context.Background(), db); err != nil {
		t.Fatalf("backfill agent turns: %v", err)
	}
}

// newAgentSessionTestRouter wires the two tool-session routes over a fresh
// migrated SQLite DB with a real RequestLogService.
func newAgentSessionTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	svc := requestlog.NewRequestLogService(db, "")
	r := gin.New()
	admin := r.Group("/api/admin")
	admin.GET("/agent-sessions", GetAgentSessions(svc))
	admin.GET("/agent-sessions/:sessionId", GetAgentSessionDetail(svc))
	return r, db
}

// sessionListItem mirrors AgentSessionListItem's JSON shape.
type sessionListItem struct {
	AgentSessionID   string    `json:"agent_session_id"`
	AgentClient      string    `json:"agent_client"`
	RequestCount     int64     `json:"request_count"`
	SuccessCount     int64     `json:"success_count"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	KnownCostMicros  int64     `json:"known_cost_micros"`
	UnknownCostCount int64     `json:"unknown_cost_count"`
	FirstSeenAt      time.Time `json:"first_seen_at"`
	LastSeenAt       time.Time `json:"last_seen_at"`
}

// sessionDetail mirrors AgentSessionDetail's JSON shape, reusing the
// listItem row shape the request-log tests already defined.
type sessionDetail struct {
	AgentSessionID string     `json:"agent_session_id"`
	AgentClient    string     `json:"agent_client"`
	Requests       []listItem `json:"requests"`
}

// seedSessionWorld seeds two sessions plus a header-less row:
//
//	hs-a (claude-code): one success row and one unknown-cost row — the
//	    hand-computed aggregate is count 2, success 1, in 110, out 220,
//	    known micros 1500, unknown rows 1, first +0m, last +10m.
//	hs-b (codex): one later success row (newer last activity → listed
//	    first): count 1, success 1, in 30, out 40, known micros 500.
//	header-less row (gemini-cli, no session): forms no session.
func seedSessionWorld(t *testing.T, db *gorm.DB) time.Time {
	t.Helper()
	base := time.Date(2026, 9, 29, 14, 0, 0, 0, time.UTC)
	claude := "claude-code"
	codex := "codex"
	gemini := "gemini-cli"

	seed := func(requestID string, ts time.Time, client, session *string, mut func(*model.RequestLog)) {
		t.Helper()
		testutil.SeedRequestLog(t, db, requestID, ts, func(r *model.RequestLog) {
			r.AgentClient = client
			r.AgentSessionID = session
			mut(r)
		})
	}
	sessA := "hs-a"
	seed("req-hs-a1", base, &claude, &sessA, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 100, 200, 1500, true
	})
	seed("req-hs-a2", base.Add(10*time.Minute), &claude, &sessA, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 429, 10, 20, 0, false
	})
	sessB := "hs-b"
	seed("req-hs-b1", base.Add(time.Hour), &codex, &sessB, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 30, 40, 500, true
	})
	seed("req-hs-none", base.Add(2*time.Hour), &gemini, nil, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 1, 1, 1, true
	})
	return base
}

// listSessions fetches the list endpoint and decodes its page envelope.
func listSessions(t *testing.T, r *gin.Engine, query string) (items []sessionListItem, total int64, page, pageSize int) {
	t.Helper()
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/agent-sessions"+query, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list %s: expected 200, got %d, body: %s", query, w.Code, w.Body.String())
	}
	var p struct {
		Total    int64             `json:"total"`
		List     []sessionListItem `json:"list"`
		Page     int               `json:"page"`
		PageSize int               `json:"page_size"`
	}
	if err := json.Unmarshal(env.Data, &p); err != nil {
		t.Fatalf("unmarshal page: %v", err)
	}
	return p.List, p.Total, p.Page, p.PageSize
}

// TestGetAgentSessionsListsAggregates pins the wire shape end to end: the
// page envelope, the aggregate JSON keys with hand-computed values for
// hs-a, the header-less row absent, and the most-recent-activity order.
func TestGetAgentSessionsListsAggregates(t *testing.T) {
	r, db := newAgentSessionTestRouter(t)
	base := seedSessionWorld(t, db)

	items, total, _, _ := listSessions(t, r, "")
	if total != 2 || len(items) != 2 {
		t.Fatalf("expected 2 sessions, got total=%d items=%d", total, len(items))
	}
	if items[0].AgentSessionID != "hs-b" || items[1].AgentSessionID != "hs-a" {
		t.Fatalf("order = [%s %s], want [hs-b hs-a] (recent activity first)", items[0].AgentSessionID, items[1].AgentSessionID)
	}
	a := items[1]
	if a.AgentClient != "claude-code" {
		t.Errorf("hs-a agent_client = %q, want claude-code", a.AgentClient)
	}
	if a.RequestCount != 2 || a.SuccessCount != 1 {
		t.Errorf("hs-a counts = (%d, %d), want (2 requests, 1 success)", a.RequestCount, a.SuccessCount)
	}
	if a.InputTokens != 110 || a.OutputTokens != 220 {
		t.Errorf("hs-a tokens = (%d, %d), want (110, 220)", a.InputTokens, a.OutputTokens)
	}
	if a.KnownCostMicros != 1500 || a.UnknownCostCount != 1 {
		t.Errorf("hs-a cost = (%d micros, %d unknown), want (1500, 1)", a.KnownCostMicros, a.UnknownCostCount)
	}
	if want := base; !a.FirstSeenAt.Equal(want) {
		t.Errorf("hs-a first_seen_at = %v, want %v", a.FirstSeenAt, want)
	}
	if want := base.Add(10 * time.Minute); !a.LastSeenAt.Equal(want) {
		t.Errorf("hs-a last_seen_at = %v, want %v", a.LastSeenAt, want)
	}
}

// TestGetAgentSessionsFiltersByClient pins the agent_client query param:
// exact match on attribution, present-but-empty matches nothing, absent is
// filter off.
func TestGetAgentSessionsFiltersByClient(t *testing.T) {
	r, db := newAgentSessionTestRouter(t)
	seedSessionWorld(t, db)

	filtered := func(query string) ([]string, int64) {
		t.Helper()
		items, total, _, _ := listSessions(t, r, query)
		ids := make([]string, 0, len(items))
		for i := range items {
			ids = append(ids, items[i].AgentSessionID)
		}
		return ids, total
	}
	if ids, total := filtered("?agent_client=claude-code"); total != 1 || len(ids) != 1 || ids[0] != "hs-a" {
		t.Fatalf("agent_client=claude-code = (%v, total=%d), want [hs-a]", ids, total)
	}
	if ids, total := filtered("?agent_client="); total != 0 || len(ids) != 0 {
		t.Fatalf("empty agent_client = (%v, total=%d), want none", ids, total)
	}
	if ids, total := filtered(""); total != 2 || len(ids) != 2 {
		t.Fatalf("absent agent_client = (%v, total=%d), want both", ids, total)
	}
}

// TestGetAgentSessionsPaginates pins the server-side pagination contract on
// the wire: page/page_size drive slicing, the total spans the whole filter,
// and the envelope echoes the effective page values.
func TestGetAgentSessionsPaginates(t *testing.T) {
	r, db := newAgentSessionTestRouter(t)
	seedSessionWorld(t, db)

	items, total, page, pageSize := listSessions(t, r, "?page=1&page_size=1")
	if total != 2 || len(items) != 1 || items[0].AgentSessionID != "hs-b" {
		t.Fatalf("page 1 size 1 = (%d items of total %d), want hs-b only", len(items), total)
	}
	if page != 1 || pageSize != 1 {
		t.Fatalf("echoed page/page_size = %d/%d, want 1/1", page, pageSize)
	}
	items, total, _, _ = listSessions(t, r, "?page=2&page_size=1")
	if total != 2 || len(items) != 1 || items[0].AgentSessionID != "hs-a" {
		t.Fatalf("page 2 size 1 = (%d items of total %d), want hs-a only", len(items), total)
	}
}

// TestGetAgentSessionDetailReturnsTimeline pins the detail envelope: the
// session's rows in chronological order carrying status_class, and the 404
// domain envelope for an unknown session id.
func TestGetAgentSessionDetailReturnsTimeline(t *testing.T) {
	r, db := newAgentSessionTestRouter(t)
	seedSessionWorld(t, db)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/agent-sessions/hs-a", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("detail hs-a: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var d sessionDetail
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if d.AgentSessionID != "hs-a" || d.AgentClient != "claude-code" {
		t.Fatalf("detail identity = (%s, %q), want (hs-a, claude-code)", d.AgentSessionID, d.AgentClient)
	}
	if len(d.Requests) != 2 {
		t.Fatalf("detail requests = %d rows, want 2", len(d.Requests))
	}
	if d.Requests[0].RequestID != "req-hs-a1" || d.Requests[0].StatusClass != "success" {
		t.Fatalf("first timeline row = (%s, %s), want (req-hs-a1, success)", d.Requests[0].RequestID, d.Requests[0].StatusClass)
	}
	if d.Requests[1].RequestID != "req-hs-a2" || d.Requests[1].StatusClass != "rejected" {
		t.Fatalf("second timeline row = (%s, %s), want (req-hs-a2, rejected)", d.Requests[1].RequestID, d.Requests[1].StatusClass)
	}

	w2, env2 := doJSON(t, r, http.MethodGet, "/api/admin/agent-sessions/nope", nil, nil)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("unknown session: expected 404, got %d, body: %s", w2.Code, w2.Body.String())
	}
	if env2.Code != errcode.AgentSessionNotFound {
		t.Fatalf("unknown session code = %d, want %d", env2.Code, errcode.AgentSessionNotFound)
	}
}

// newAgentSessionTranscriptRouter wires the transcript routes (plus the list
// route, so a test can cross-check the transcript's session aggregate against
// the list row the same SQL family produces) over a fresh migrated SQLite DB
// with a real RequestLogService bound to the given bodiesDir.
func newAgentSessionTranscriptRouter(t *testing.T, bodiesDir string) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	svc := requestlog.NewRequestLogService(db, bodiesDir)
	r := gin.New()
	admin := r.Group("/api/admin")
	admin.GET("/agent-sessions", GetAgentSessions(svc))
	admin.GET("/agent-sessions/:sessionId/transcript", GetAgentSessionTranscript(svc))
	admin.GET("/agent-sessions/:sessionId/transcript/system", GetAgentSessionTranscriptSystem(svc))
	return r, db
}

// transcriptAssistant mirrors AgentSessionReplayAssistant's JSON shape.
type transcriptAssistant struct {
	Source    string `json:"source"`
	Truncated bool   `json:"truncated"`
	Raw       any    `json:"raw"`
}

// transcriptBlock mirrors AgentSessionReplayBlock's JSON shape.
type transcriptBlock struct {
	RequestID      string                `json:"request_id"`
	CreatedAt      time.Time             `json:"created_at"`
	ModelName      string                `json:"model_name"`
	StatusClass    string                `json:"status_class"`
	IsStream       bool                  `json:"is_stream"`
	InputTokens    int                   `json:"input_tokens"`
	OutputTokens   int                   `json:"output_tokens"`
	DurationMs     int64                 `json:"duration_ms"`
	ProviderName   string                `json:"provider_name"`
	Added          []any                 `json:"added"`
	System         *transcriptSystemMeta `json:"system"`
	Assistant      transcriptAssistant   `json:"assistant"`
	AddedTruncated bool                  `json:"added_truncated"`
}

// transcriptSystemMeta mirrors AgentSessionReplaySystemMeta's JSON shape —
// the transcript endpoint's metadata-only system entry (no raw).
type transcriptSystemMeta struct {
	SeenAtRequest string `json:"seen_at_request"`
	Truncated     bool   `json:"truncated"`
	RawLength     int    `json:"raw_length"`
}

// transcriptSystemRaw mirrors AgentSessionReplaySystem's JSON shape — the
// /transcript/system endpoint's entry carrying the raw value.
type transcriptSystemRaw struct {
	SeenAtRequest string `json:"seen_at_request"`
	Truncated     bool   `json:"truncated"`
	Raw           any    `json:"raw"`
}

// transcriptPayload mirrors AgentSessionTranscript's JSON shape, reusing
// sessionListItem for the session half.
type transcriptPayload struct {
	Session       sessionListItem        `json:"session"`
	System        []transcriptSystemMeta `json:"system"`
	Blocks        []transcriptBlock      `json:"blocks"`
	HasMoreBefore bool                   `json:"has_more_before"`
	NextBefore    string                 `json:"next_before"`
	HasMoreAfter  bool                   `json:"has_more_after"`
	NextAfter     string                 `json:"next_after"`
}

// fetchTranscript GETs the transcript endpoint with an optional query string
// and decodes its payload.
func fetchTranscript(t *testing.T, r *gin.Engine, sessionID, query string) (*httptest.ResponseRecorder, transcriptPayload) {
	t.Helper()
	path := "/api/admin/agent-sessions/" + sessionID + "/transcript"
	if query != "" {
		path += "?" + query
	}
	w, env := doJSON(t, r, http.MethodGet, path, nil, nil)
	var p transcriptPayload
	if err := json.Unmarshal(env.Data, &p); err != nil {
		t.Fatalf("unmarshal transcript: %v (body: %s)", err, w.Body.String())
	}
	return w, p
}

// TestGetAgentSessionTranscriptReturnsReplay pins the transcript endpoint's
// 200 contract on a real-shaped session mixing success / failure / stream /
// unrecorded turns: the session aggregate (hand-computed AND cross-checked
// against the list endpoint's SQL row), the deduplicated system timeline, the
// per-request blocks in (created_at, id) ASC order with their metadata, added
// messages and assistant attribution (all four sources), and the 14006
// envelope for an unknown session.
func TestGetAgentSessionTranscriptReturnsReplay(t *testing.T) {
	bodiesDir := t.TempDir()
	r, db := newAgentSessionTranscriptRouter(t, bodiesDir)
	base := time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC)
	claude := "claude-code"

	alpha := testutil.SeedProvider(t, db, "p-alpha")
	beta := testutil.SeedProvider(t, db, "p-beta")

	seed := func(requestID string, ts time.Time, provider *uint, modelName string, statusCode int, in, out int, cost int64, known bool, dur int64, isStream bool) {
		t.Helper()
		testutil.SeedRequestLog(t, db, requestID, ts, func(row *model.RequestLog) {
			row.AgentClient = &claude
			sess := "hs-t"
			row.AgentSessionID = &sess
			row.ProviderID = provider
			row.ModelName = modelName
			row.StatusCode = statusCode
			row.InputTokens = in
			row.OutputTokens = out
			row.CostMicros = cost
			row.CostKnown = known
			row.DurationMs = dur
			row.IsStream = isStream
		})
	}
	seedBody := func(requestID, reqBody, respBody, streamPath string) {
		t.Helper()
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:      requestID,
			RequestBody:    reqBody,
			ResponseBody:   respBody,
			StreamBodyPath: streamPath,
		}); err != nil {
			t.Fatalf("upsert body %s: %v", requestID, err)
		}
	}

	// t1 — success, non-stream, inline reply recorded, system present.
	const t1Req = `{"system":"You are a coder.","messages":[{"role":"user","content":"hi"}]}`
	const t1Resp = `{"content":[{"type":"text","text":"hello"}]}`
	seed("req-t1", base, &alpha, "gpt-4o-mini", 200, 100, 200, 1500, true, 42, false)
	seedBody("req-t1", t1Req, t1Resp, "")

	// t2 — success, STREAM: reply lives only in the capture file under
	// bodiesDir; carries an unknown cost.
	const t2Req = `{"system":"You are a coder.","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"do a thing"}]}`
	const t2Stream = "data: {\"delta\":\"d\"}\n\ndata: [DONE]\n\n"
	seed("req-t2", base.Add(time.Minute), &beta, "claude-3-7", 200, 110, 220, 0, false, 60, true)
	if err := os.WriteFile(filepath.Join(bodiesDir, "req-t2.stream"), []byte(t2Stream), 0o600); err != nil {
		t.Fatalf("write capture file: %v", err)
	}
	seedBody("req-t2", t2Req, "", "req-t2.stream")

	// t3 — 429 failure: request recorded, but neither reply body nor capture.
	const t3Req = `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"do a thing"},{"role":"assistant","content":"I did it"},{"role":"user","content":"show me"}]}`
	seed("req-t3", base.Add(2*time.Minute), nil, "gpt-4o-mini", 429, 10, 20, 0, false, 5, false)
	seedBody("req-t3", t3Req, "", "")

	// t4 — success; its request resends the assistant copy that becomes t3's
	// reply attribution (resend source), and its own reply is inline.
	const t4Req = `{"system":"You are a coder.","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"do a thing"},{"role":"assistant","content":"I did it"},{"role":"user","content":"show me"},{"role":"assistant","content":"here you go"},{"role":"user","content":"and now?"}]}`
	const t4Resp = `{"content":[{"type":"text","text":"done"}]}`
	seed("req-t4", base.Add(3*time.Minute), &alpha, "gpt-4o-mini", 200, 5, 7, 100, true, 30, false)
	seedBody("req-t4", t4Req, t4Resp, "")

	// t5 — 499 cancelled, no body row at all (capture failure), SAME
	// created_at as t4: pins the id ASC tie-break of the block order.
	seed("req-t5", base.Add(3*time.Minute), nil, "claude-3-7", 499, 1, 1, 0, false, 2, true)
	backfillAgentTurns(t, db)

	w, p := fetchTranscript(t, r, "hs-t", "")
	if w.Code != http.StatusOK {
		t.Fatalf("transcript hs-t: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}

	// Session aggregate — hand-computed mirrors of the list SQL semantics.
	s := p.Session
	if s.AgentSessionID != "hs-t" || s.AgentClient != "claude-code" {
		t.Fatalf("session identity = (%s, %q), want (hs-t, claude-code)", s.AgentSessionID, s.AgentClient)
	}
	if s.RequestCount != 5 || s.SuccessCount != 3 {
		t.Fatalf("counts = (%d requests, %d success), want (5, 3)", s.RequestCount, s.SuccessCount)
	}
	if s.InputTokens != 226 || s.OutputTokens != 448 {
		t.Fatalf("tokens = (%d, %d), want (226, 448)", s.InputTokens, s.OutputTokens)
	}
	if s.KnownCostMicros != 1600 || s.UnknownCostCount != 3 {
		t.Fatalf("cost = (%d micros, %d unknown), want (1600, 3)", s.KnownCostMicros, s.UnknownCostCount)
	}
	if !s.FirstSeenAt.Equal(base) || !s.LastSeenAt.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("seen window = (%v, %v), want (%v, %v)", s.FirstSeenAt, s.LastSeenAt, base, base.Add(3*time.Minute))
	}

	// Cross-check: the transcript's session aggregate must equal the LIST
	// row's SQL aggregate for the same session — the two are one contract.
	items, _, _, _ := listSessions(t, r, "")
	var listRow *sessionListItem
	for i := range items {
		if items[i].AgentSessionID == "hs-t" {
			listRow = &items[i]
		}
	}
	if listRow == nil {
		t.Fatalf("list endpoint carries no hs-t row to cross-check against")
	}
	if s.RequestCount != listRow.RequestCount || s.SuccessCount != listRow.SuccessCount ||
		s.InputTokens != listRow.InputTokens || s.OutputTokens != listRow.OutputTokens ||
		s.KnownCostMicros != listRow.KnownCostMicros || s.UnknownCostCount != listRow.UnknownCostCount ||
		!s.FirstSeenAt.Equal(listRow.FirstSeenAt) || !s.LastSeenAt.Equal(listRow.LastSeenAt) {
		t.Fatalf("transcript session %+v disagrees with list row %+v", s, *listRow)
	}

	// System timeline METADATA: one entry, from the first request, carrying
	// the raw value's serialized size — the raw text itself only ships from
	// the /transcript/system endpoint (asserted in the alignment test below).
	if len(p.System) != 1 {
		t.Fatalf("system timeline = %d entries, want 1: %+v", len(p.System), p.System)
	}
	if p.System[0].SeenAtRequest != "req-t1" || p.System[0].Truncated {
		t.Fatalf("system[0] = (%s, truncated=%v), want (req-t1, false)", p.System[0].SeenAtRequest, p.System[0].Truncated)
	}
	if want := len(`"You are a coder."`); p.System[0].RawLength != want {
		t.Fatalf("system[0].raw_length = %d, want %d (the JSON-serialized size of the prompt)", p.System[0].RawLength, want)
	}

	// Blocks: EXACTLY one per request — count first, so a duplicated or
	// dropped block fails here instead of hiding behind the per-index checks
	// below (the per-index loop only walks want's five entries and would
	// never notice a sixth block).
	if len(p.Blocks) != 5 {
		t.Fatalf("blocks = %d, want exactly 5 (one per request, no duplicates)", len(p.Blocks))
	}
	// Blocks: one per request, (created_at, id) ASC — t4/t5 share a
	// timestamp, so their order pins the id tie-break — each block carrying
	// its own created_at (the per-request timeline metadata TC-06 lists).
	order := make([]string, 0, len(p.Blocks))
	for i := range p.Blocks {
		order = append(order, p.Blocks[i].RequestID)
	}
	want := []string{"req-t1", "req-t2", "req-t3", "req-t4", "req-t5"}
	wantAt := []time.Time{
		base, base.Add(time.Minute), base.Add(2 * time.Minute),
		base.Add(3 * time.Minute), base.Add(3 * time.Minute),
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("block order = %v, want %v", order, want)
		}
		if !p.Blocks[i].CreatedAt.Equal(wantAt[i]) {
			t.Fatalf("block %s created_at = %v, want %v", want[i], p.Blocks[i].CreatedAt, wantAt[i])
		}
	}

	// Block metadata + added + assistant attribution, per hand-computed
	// expectations (the dedup pass itself is table-tested in the service
	// package; here we pin what the endpoint wires into it).
	b1 := p.Blocks[1]
	if b1.ModelName != "claude-3-7" || b1.ProviderName != "p-beta" || !b1.IsStream {
		t.Fatalf("t2 metadata = (%s, %q, stream=%v), want (claude-3-7, p-beta, true)", b1.ModelName, b1.ProviderName, b1.IsStream)
	}
	if b1.StatusClass != "success" || b1.DurationMs != 60 || b1.InputTokens != 110 || b1.OutputTokens != 220 {
		t.Fatalf("t2 row figures = (%s, %dms, %d/%d tokens), want (success, 60ms, 110/220)", b1.StatusClass, b1.DurationMs, b1.InputTokens, b1.OutputTokens)
	}
	if len(b1.Added) != 1 || b1.Added[0].(map[string]any)["content"] != "do a thing" {
		t.Fatalf("t2 added = %+v, want the one new user message", b1.Added)
	}
	if b1.Assistant.Source != "stream" || b1.Assistant.Truncated || b1.Assistant.Raw != t2Stream {
		t.Fatalf("t2 assistant = %+v, want (stream, false, capture text)", b1.Assistant)
	}

	b0 := p.Blocks[0]
	if b0.Assistant.Source != "inline" || b0.Assistant.Raw != t1Resp || b0.AddedTruncated {
		t.Fatalf("t1 assistant = %+v (added_truncated=%v), want (inline, t1Resp, false)", b0.Assistant, b0.AddedTruncated)
	}
	if len(b0.Added) != 1 || b0.Added[0].(map[string]any)["role"] != "user" {
		t.Fatalf("t1 added = %+v, want the one user message", b0.Added)
	}

	b2 := p.Blocks[2]
	if b2.StatusClass != "rejected" || b2.ProviderName != "" {
		t.Fatalf("t3 = (%s, provider %q), want (rejected, \"\")", b2.StatusClass, b2.ProviderName)
	}
	if b2.Assistant.Source != "resend" {
		t.Fatalf("t3 assistant source = %q, want resend", b2.Assistant.Source)
	}
	copies, ok := b2.Assistant.Raw.([]any)
	if !ok || len(copies) != 1 || copies[0].(map[string]any)["content"] != "here you go" {
		t.Fatalf("t3 resend raw = %+v, want the one resent assistant copy", b2.Assistant.Raw)
	}
	if len(b2.Added) != 1 || b2.Added[0].(map[string]any)["content"] != "show me" {
		t.Fatalf("t3 added = %+v, want the one new user message", b2.Added)
	}

	b3 := p.Blocks[3]
	if b3.Assistant.Source != "inline" || b3.Assistant.Raw != t4Resp {
		t.Fatalf("t4 assistant = %+v, want (inline, t4Resp)", b3.Assistant)
	}

	b4 := p.Blocks[4]
	if b4.StatusClass != "cancelled" || b4.Assistant.Source != "none" || b4.Assistant.Raw != nil {
		t.Fatalf("t5 = (%s, assistant %+v), want (cancelled, source none, raw nil)", b4.StatusClass, b4.Assistant)
	}
	if len(b4.Added) != 0 {
		t.Fatalf("t5 added = %+v, want empty (no body row)", b4.Added)
	}

	// Unknown session → the domain 404 envelope, code 14006.
	w2, env2 := doJSON(t, r, http.MethodGet, "/api/admin/agent-sessions/nope/transcript", nil, nil)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("unknown session transcript: expected 404, got %d, body: %s", w2.Code, w2.Body.String())
	}
	if env2.Code != errcode.AgentSessionNotFound {
		t.Fatalf("unknown session transcript code = %d, want %d", env2.Code, errcode.AgentSessionNotFound)
	}
}

// TestGetAgentSessionTranscriptDegradesWithoutBodiesDir pins the empty-
// bodiesDir degradation: no capture can be inlined (stream is always empty),
// so streaming turns attribute via their inline body when one exists and via
// resend/none otherwise — every block still present, no error envelope.
func TestGetAgentSessionTranscriptDegradesWithoutBodiesDir(t *testing.T) {
	r, db := newAgentSessionTranscriptRouter(t, "")
	base := time.Date(2026, 10, 9, 16, 0, 0, 0, time.UTC)
	codex := "codex"

	seed := func(requestID string, minutes int) {
		t.Helper()
		testutil.SeedRequestLog(t, db, requestID, base.Add(time.Duration(minutes)*time.Minute), func(row *model.RequestLog) {
			row.AgentClient = &codex
			sess := "hs-e"
			row.AgentSessionID = &sess
			row.IsStream = true
			row.StatusCode = 200
		})
	}
	seedBody := func(requestID, reqBody, respBody string) {
		t.Helper()
		// stream_body_path is set on every row — the empty bodiesDir is what
		// makes the read come back empty, not a missing column.
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:      requestID,
			RequestBody:    reqBody,
			ResponseBody:   respBody,
			StreamBodyPath: requestID + ".stream",
		}); err != nil {
			t.Fatalf("upsert body %s: %v", requestID, err)
		}
	}

	seed("req-e1", 0)
	seedBody("req-e1",
		`{"messages":[{"role":"user","content":"q1"}]}`,
		`{"content":[{"type":"text","text":"r1"}]}`)
	seed("req-e2", 1)
	seedBody("req-e2",
		`{"messages":[{"role":"user","content":"q1"},{"role":"assistant","content":"r1"},{"role":"user","content":"q2"}]}`,
		"")
	seed("req-e3", 2)
	seedBody("req-e3",
		`{"messages":[{"role":"user","content":"q1"},{"role":"assistant","content":"r1"},{"role":"user","content":"q2"},{"role":"assistant","content":"r2"},{"role":"user","content":"q3"}]}`,
		"")
	backfillAgentTurns(t, db)

	w, p := fetchTranscript(t, r, "hs-e", "")
	if w.Code != http.StatusOK {
		t.Fatalf("transcript hs-e: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if len(p.Blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (no block may go missing)", len(p.Blocks))
	}
	sources := []string{p.Blocks[0].Assistant.Source, p.Blocks[1].Assistant.Source, p.Blocks[2].Assistant.Source}
	want := []string{"inline", "resend", "none"}
	for i := range want {
		if sources[i] != want[i] {
			t.Fatalf("assistant sources = %v, want %v", sources, want)
		}
	}
	if p.Blocks[0].Assistant.Raw != `{"content":[{"type":"text","text":"r1"}]}` {
		t.Fatalf("e1 inline raw = %v, want the inline response body", p.Blocks[0].Assistant.Raw)
	}
	if copies, ok := p.Blocks[1].Assistant.Raw.([]any); !ok || len(copies) != 1 || copies[0].(map[string]any)["content"] != "r2" {
		t.Fatalf("e2 resend raw = %+v, want the one resent assistant copy", p.Blocks[1].Assistant.Raw)
	}
	if p.Blocks[2].Assistant.Raw != nil {
		t.Fatalf("e3 raw = %v, want nil (nothing recorded, nothing to resend)", p.Blocks[2].Assistant.Raw)
	}
	if p.Session.RequestCount != 3 || p.Session.SuccessCount != 3 {
		t.Fatalf("session counts = (%d, %d), want (3, 3)", p.Session.RequestCount, p.Session.SuccessCount)
	}
}

// transcriptInlineCap mirrors requestlog.maxInlineBodyBytes (1 MiB) — the
// cap the transcript's inline bodies and stream reads share with the detail
// endpoint. Duplicated as a literal because the constant is unexported; if
// the cap ever moves, these fixtures stop being over-cap and this test goes
// red, which is exactly the drift signal wanted.
const transcriptInlineCap = 1 << 20

// TestGetAgentSessionTranscriptMarksTruncation pins the truncation-flag
// WIRING between stored bodies and the replay payload on over-cap (>1 MiB)
// fixtures: the len>cap derivations for the inline request and response
// bodies, and BOTH halves of the stream OR — the read-side inline cap raised
// by an over-cap capture file, and the write-side disk backstop flag the
// body row recorded on an under-cap file. Every capped copy is also checked
// against its original (prefix kept, shorter, visible marker), so the text
// and the flag cannot disagree silently.
func TestGetAgentSessionTranscriptMarksTruncation(t *testing.T) {
	bodiesDir := t.TempDir()
	r, db := newAgentSessionTranscriptRouter(t, bodiesDir)
	base := time.Date(2026, 10, 9, 17, 0, 0, 0, time.UTC)
	claude := "claude-code"

	seed := func(requestID string, minutes int, isStream bool) {
		t.Helper()
		testutil.SeedRequestLog(t, db, requestID, base.Add(time.Duration(minutes)*time.Minute), func(row *model.RequestLog) {
			row.AgentClient = &claude
			sess := "hs-x"
			row.AgentSessionID = &sess
			row.IsStream = isStream
			row.StatusCode = 200
		})
	}
	seedBody := func(requestID, reqBody, respBody, streamPath string, streamFlag bool) {
		t.Helper()
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:           requestID,
			RequestBody:         reqBody,
			ResponseBody:        respBody,
			StreamBodyPath:      streamPath,
			StreamBodyTruncated: streamFlag,
		}); err != nil {
			t.Fatalf("upsert body %s: %v", requestID, err)
		}
	}

	// x1 — over-cap request body (framing bytes push it past the cap):
	// added_truncated=true and added degrades to empty.
	bigReq := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", transcriptInlineCap) + `"}]}`
	seed("req-x1", 0, false)
	seedBody("req-x1", bigReq, "", "", false)

	// x2 — over-cap inline response body: assistant sources inline with
	// truncated=true and a genuinely cut copy. The fixture runs to 2× the cap
	// so the cut copy (cap + marker) is unambiguously shorter than the
	// original — a body only barely over the cap would leave the two within
	// marker-length of each other.
	bigResp := `{"content":[{"type":"text","text":"` + strings.Repeat("b", 2*transcriptInlineCap) + `"}]}`
	seed("req-x2", 1, false)
	seedBody("req-x2", `{"messages":[{"role":"user","content":"q2"}]}`, bigResp, "", false)

	// x3 — over-cap stream capture FILE, write-side flag false: the read-side
	// inline cap alone must raise the marker (same 2× margin as x2).
	streamBig := "data: {\"delta\":\"" + strings.Repeat("c", 2*transcriptInlineCap) + "\"}\n\n"
	seed("req-x3", 2, true)
	if err := os.WriteFile(filepath.Join(bodiesDir, "req-x3.stream"), []byte(streamBig), 0o600); err != nil {
		t.Fatalf("write capture file: %v", err)
	}
	seedBody("req-x3", `{"messages":[{"role":"user","content":"q3"}]}`, "", "req-x3.stream", false)

	// x4 — SMALL capture file but the body row records the write-side
	// stream_body_truncated backstop: the flag must still surface, while the
	// under-cap text ships whole.
	const streamSmall = "data: {\"delta\":\"tiny\"}\n\n"
	seed("req-x4", 3, true)
	if err := os.WriteFile(filepath.Join(bodiesDir, "req-x4.stream"), []byte(streamSmall), 0o600); err != nil {
		t.Fatalf("write capture file: %v", err)
	}
	seedBody("req-x4", `{"messages":[{"role":"user","content":"q4"}]}`, "", "req-x4.stream", true)
	backfillAgentTurns(t, db)

	// Two of these blocks (x2's capped inline copy, x3's capped capture)
	// each serialize to ~1 MiB, so the shipped 1.5 MiB page budget pages the
	// fixture: the first call serves the newest fit, and the cursor walk
	// collects the rest — itself a live pin that the truncation fixtures
	// ride the byte budget exactly as the paging contract says.
	w, p := fetchTranscript(t, r, "hs-x", "")
	if w.Code != http.StatusOK {
		t.Fatalf("transcript hs-x: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	blocks := append([]transcriptBlock{}, p.Blocks...)
	for p.HasMoreBefore {
		_, p = fetchTranscript(t, r, "hs-x", "before="+p.NextBefore)
		blocks = append(append([]transcriptBlock{}, p.Blocks...), blocks...)
	}
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4", len(blocks))
	}
	for i, want := range []time.Time{base, base.Add(time.Minute), base.Add(2 * time.Minute), base.Add(3 * time.Minute)} {
		if !blocks[i].CreatedAt.Equal(want) {
			t.Fatalf("block %d created_at = %v, want %v", i, blocks[i].CreatedAt, want)
		}
	}

	// x1: the request body's len>cap derivation.
	x1 := blocks[0]
	if !x1.AddedTruncated {
		t.Fatalf("x1 added_truncated = false, want true (request body over the inline cap)")
	}
	if x1.Added == nil || len(x1.Added) != 0 {
		t.Fatalf("x1 added = %#v, want non-nil empty array (a capped body never half-parses)", x1.Added)
	}

	// x2: the response body's len>cap derivation — flag and cut text together.
	x2 := blocks[1]
	if x2.AddedTruncated {
		t.Fatalf("x2 added_truncated = true, want false (its request body fits the cap)")
	}
	if x2.Assistant.Source != "inline" || !x2.Assistant.Truncated {
		t.Fatalf("x2 assistant = (%s, truncated=%v), want (inline, true)", x2.Assistant.Source, x2.Assistant.Truncated)
	}
	resp2, ok := x2.Assistant.Raw.(string)
	if !ok {
		t.Fatalf("x2 assistant raw = %T, want string", x2.Assistant.Raw)
	}
	assertCutCopy(t, "x2 inline response", resp2, bigResp)

	// x3: read-side cap on the capture file — the capped-read half of the
	// readStreamBodyInlineCapped OR (the body row's write-side flag is
	// false here; the read alone raises the marker).
	x3 := blocks[2]
	if x3.Assistant.Source != "stream" || !x3.Assistant.Truncated {
		t.Fatalf("x3 assistant = (%s, truncated=%v), want (stream, true) from the read-side cap alone", x3.Assistant.Source, x3.Assistant.Truncated)
	}
	stream3, ok := x3.Assistant.Raw.(string)
	if !ok {
		t.Fatalf("x3 assistant raw = %T, want string", x3.Assistant.Raw)
	}
	assertCutCopy(t, "x3 stream capture", stream3, streamBig)

	// x4: write-side backstop flag on an under-cap file — the OR's other
	// half, with the small capture still shipped whole.
	x4 := blocks[3]
	if x4.Assistant.Source != "stream" || !x4.Assistant.Truncated {
		t.Fatalf("x4 assistant = %+v, want (stream, truncated=true) from the write-side backstop flag alone", x4.Assistant)
	}
	if x4.Assistant.Raw != streamSmall {
		t.Fatalf("x4 assistant raw = %v, want the full small capture %q", x4.Assistant.Raw, streamSmall)
	}
}

// assertCutCopy checks one capped inline copy against its original: it keeps
// the original's prefix, is not the whole original, and carries the visible
// truncation marker — so a raised flag always comes with text that really
// was cut, and a cut text always carries the flag's marker.
func assertCutCopy(t *testing.T, what, got, original string) {
	t.Helper()
	if !strings.HasPrefix(got, original[:4096]) {
		t.Fatalf("%s: inline copy does not start with the original text", what)
	}
	if len(got) >= len(original) {
		t.Fatalf("%s: inline copy is %d bytes, want shorter than the %d-byte original", what, len(got), len(original))
	}
	if !strings.Contains(got, "[truncated") {
		t.Fatalf("%s: inline copy carries no visible truncation marker", what)
	}
}

// seedTranscriptPageSession seeds a 25-request session (ids zero-padded,
// one minute apart) with a minimal per-request body — the cursor-pagination
// handler tests only care about which blocks a call returns.
func seedTranscriptPageSession(t *testing.T, db *gorm.DB) {
	t.Helper()
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "hs-page"
	for i := 1; i <= 25; i++ {
		id := fmt.Sprintf("req-%02d", i)
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:    id,
			RequestBody:  fmt.Sprintf(`{"messages":[{"role":"user","content":"q%d"}]}`, i),
			ResponseBody: fmt.Sprintf(`{"content":[{"type":"text","text":"r%d"}]}`, i),
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
	}
	backfillAgentTurns(t, db)
}

// transcriptBlockIDs collects a payload's block ids in order.
func transcriptBlockIDs(p transcriptPayload) []string {
	ids := make([]string, 0, len(p.Blocks))
	for i := range p.Blocks {
		ids = append(ids, p.Blocks[i].RequestID)
	}
	return ids
}

// TestGetAgentSessionTranscriptCursorWireSmoke pins that the query params
// reach the slicer with the declared semantics on the wire: the no-cursor
// call serves the newest blocks, before continues exclusively toward older,
// after anchors inclusively — the handler's plumbing around the table-tested
// pure function (TC-01/02/03's integration cross-check).
func TestGetAgentSessionTranscriptCursorWireSmoke(t *testing.T) {
	r, db := newAgentSessionTranscriptRouter(t, "")
	seedTranscriptPageSession(t, db)

	_, p := fetchTranscript(t, r, "hs-page", "limit=10")
	if got := transcriptBlockIDs(p); len(got) != 10 || got[0] != "req-16" || got[9] != "req-25" {
		t.Fatalf("latest page = %v, want req-16..req-25", got)
	}
	if !p.HasMoreBefore || p.NextBefore != "req-16" || p.HasMoreAfter {
		t.Fatalf("latest page continuation = (%v,%q,%v), want (true, req-16, false)", p.HasMoreBefore, p.NextBefore, p.HasMoreAfter)
	}

	_, p = fetchTranscript(t, r, "hs-page", "limit=10&before=req-16")
	if got := transcriptBlockIDs(p); len(got) != 10 || got[0] != "req-06" || got[9] != "req-15" {
		t.Fatalf("before page = %v, want req-06..req-15 (cursor block excluded)", got)
	}

	_, p = fetchTranscript(t, r, "hs-page", "limit=5&after=req-03")
	if got := transcriptBlockIDs(p); len(got) != 5 || got[0] != "req-03" || got[4] != "req-07" {
		t.Fatalf("after page = %v, want req-03..req-07 (cursor block included)", got)
	}
	if !p.HasMoreAfter || p.NextAfter != "req-07" {
		t.Fatalf("after page continuation = (%v, %q), want (true, req-07)", p.HasMoreAfter, p.NextAfter)
	}
}

// TestGetAgentSessionTranscriptParamBounds pins TC-04 on the wire: limit
// fallback (non-numeric / zero / over-max all behave as the default 20; 200
// itself is valid), the lenient bad-cursor empty page, and the 400 for
// before+after together.
func TestGetAgentSessionTranscriptParamBounds(t *testing.T) {
	r, db := newAgentSessionTranscriptRouter(t, "")
	seedTranscriptPageSession(t, db)

	for _, q := range []string{"limit=abc", "limit=0", "limit=999999", ""} {
		_, p := fetchTranscript(t, r, "hs-page", q)
		if len(p.Blocks) != 20 || p.Blocks[0].RequestID != "req-06" {
			t.Fatalf("query %q: blocks = %d starting at %s, want 20 starting at req-06 (default limit)", q, len(p.Blocks), p.Blocks[0].RequestID)
		}
	}
	_, p := fetchTranscript(t, r, "hs-page", "limit=200")
	if len(p.Blocks) != 25 {
		t.Fatalf("limit=200: blocks = %d, want all 25 (the cap is a valid size, not a clamp to 20)", len(p.Blocks))
	}

	// Bad cursors: unknown ids answer an empty page with both has_more false
	// — a stale deep link must read as "nothing left", not an error.
	for _, q := range []string{"before=req-nope", "after=req-nope", "limit=5&before=gone-id"} {
		w, p := fetchTranscript(t, r, "hs-page", q)
		if w.Code != http.StatusOK {
			t.Fatalf("query %q: status %d, want 200", q, w.Code)
		}
		if p.Blocks == nil || len(p.Blocks) != 0 {
			t.Fatalf("query %q: blocks = %#v, want a non-nil empty array", q, p.Blocks)
		}
		if p.HasMoreBefore || p.HasMoreAfter || p.NextBefore != "" || p.NextAfter != "" {
			t.Fatalf("query %q: continuation = (%v,%q,%v,%q), want all clear", q, p.HasMoreBefore, p.NextBefore, p.HasMoreAfter, p.NextAfter)
		}
		// The session half still arrives — the page is empty, not the session.
		if p.Session.RequestCount != 25 {
			t.Fatalf("query %q: session request_count = %d, want 25", q, p.Session.RequestCount)
		}
	}

	// before+after together: one call pages one way — 400 ParamError.
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/agent-sessions/hs-page/transcript?before=req-05&after=req-10", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("before+after: status %d, want 400, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.InvalidParam {
		t.Fatalf("before+after: code = %d, want %d (InvalidParam)", env.Code, errcode.InvalidParam)
	}
}

// TestGetAgentSessionTranscriptSystemAlignment pins TC-08: the transcript's
// system METADATA and the /transcript/system raw timeline are two views of
// one assembly — index-aligned entry counts, seen_at_request sequences,
// truncated flags, raw_length == each raw value's UTF-8 byte count as
// serialized — and the transcript response carries no system raw key at all.
func TestGetAgentSessionTranscriptSystemAlignment(t *testing.T) {
	r, db := newAgentSessionTranscriptRouter(t, "")
	base := time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "hs-align"

	for i, sys := range []string{"align-one", "align-two", "align-two"} {
		id := fmt.Sprintf("req-a%d", i+1)
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(row *model.RequestLog) {
			row.AgentClient = &claude
			row.AgentSessionID = &sess
			row.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:    id,
			RequestBody:  fmt.Sprintf(`{"system":%q,"messages":[{"role":"user","content":"q"}]}`, sys),
			ResponseBody: `{"content":[{"type":"text","text":"r"}]}`,
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
	}
	backfillAgentTurns(t, db)

	_, p := fetchTranscript(t, r, sess, "")
	if len(p.System) != 2 {
		t.Fatalf("transcript system metadata = %d entries, want 2 (deduplicated): %+v", len(p.System), p.System)
	}
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/agent-sessions/"+sess+"/transcript/system", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("transcript/system: status %d, body: %s", w.Code, w.Body.String())
	}
	var sysPayload struct {
		System []transcriptSystemRaw `json:"system"`
	}
	if err := json.Unmarshal(env.Data, &sysPayload); err != nil {
		t.Fatalf("unmarshal transcript/system: %v (body: %s)", err, w.Body.String())
	}
	if len(sysPayload.System) != len(p.System) {
		t.Fatalf("entry counts = raw %d / metadata %d, want equal (index-aligned halves)", len(sysPayload.System), len(p.System))
	}
	for i := range sysPayload.System {
		raw, meta := sysPayload.System[i], p.System[i]
		if raw.SeenAtRequest != meta.SeenAtRequest {
			t.Fatalf("entry %d seen_at_request = raw %q / metadata %q, want equal", i, raw.SeenAtRequest, meta.SeenAtRequest)
		}
		if raw.Truncated != meta.Truncated {
			t.Fatalf("entry %d truncated = raw %v / metadata %v, want equal", i, raw.Truncated, meta.Truncated)
		}
		serialized, err := json.Marshal(raw.Raw)
		if err != nil {
			t.Fatalf("marshal raw %d: %v", i, err)
		}
		if meta.RawLength != len(serialized) {
			t.Fatalf("entry %d raw_length = %d, want %d (the raw's serialized UTF-8 bytes)", i, meta.RawLength, len(serialized))
		}
	}
	if sysPayload.System[0].Raw != "align-one" || sysPayload.System[1].Raw != "align-two" {
		t.Fatalf("raw values = (%v, %v), want the two prompts in order", sysPayload.System[0].Raw, sysPayload.System[1].Raw)
	}

	// The transcript payload must not carry the raw key anywhere in system.
	var probe struct {
		System []map[string]json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal(env2Data(t, r, sess), &probe); err != nil {
		t.Fatalf("unmarshal transcript probe: %v", err)
	}
	for i, entry := range probe.System {
		if _, has := entry["raw"]; has {
			t.Fatalf("transcript system[%d] carries a raw key — the raw text belongs to /transcript/system only", i)
		}
		for _, key := range []string{"seen_at_request", "truncated", "raw_length"} {
			if _, has := entry[key]; !has {
				t.Fatalf("transcript system[%d] lacks %q", i, key)
			}
		}
	}
}

// env2Data fetches the transcript endpoint's raw data JSON for key-level
// probes (mirror structs cannot see keys that must be ABSENT).
func env2Data(t *testing.T, r *gin.Engine, sessionID string) json.RawMessage {
	t.Helper()
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/agent-sessions/"+sessionID+"/transcript", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("transcript %s: status %d, body: %s", sessionID, w.Code, w.Body.String())
	}
	return env.Data
}

// TestGetAgentSessionTranscriptSystemPerRequest pins the ?request= form on
// the wire: the block's own system metadata (the per-request collapse row's
// data hook) ships metadata-only inside the transcript, and the raw fetch
// resolves to the value in effect at the named request — its own entry when
// the value changed there, the inherited one when it repeated it, the empty
// timeline for an id that names no request of the session (lenient, like
// the stale cursors), and 14006 for an unknown session with the param.
func TestGetAgentSessionTranscriptSystemPerRequest(t *testing.T) {
	r, db := newAgentSessionTranscriptRouter(t, "")
	base := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "hs-pr"

	bodies := map[string]string{
		"p0": `{"messages":[{"role":"user","content":"q0"}]}`,
		"p1": `{"system":"sys-one","messages":[{"role":"user","content":"q1"}]}`,
		"p2": `{"system":"sys-one","messages":[{"role":"user","content":"q2"}]}`,
		"p3": `{"system":"sys-two","messages":[{"role":"user","content":"q3"}]}`,
	}
	order := []string{"p0", "p1", "p2", "p3"}
	for i, id := range order {
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(row *model.RequestLog) {
			row.AgentClient = &claude
			row.AgentSessionID = &sess
			row.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:   id,
			RequestBody: bodies[id],
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
	}
	backfillAgentTurns(t, db)

	// The transcript's blocks carry the per-request metadata — present for
	// requests with a value in effect, absent for the request that precedes
	// every recorded entry, and never carrying raw text.
	_, p := fetchTranscript(t, r, sess, "")
	if len(p.Blocks) != 4 {
		t.Fatalf("blocks = %d, want 4", len(p.Blocks))
	}
	if p.Blocks[0].System != nil {
		t.Fatalf("p0 block System = %+v, want nil (nothing recorded yet)", p.Blocks[0].System)
	}
	if p.Blocks[1].System == nil || p.Blocks[1].System.SeenAtRequest != "p1" || p.Blocks[1].System.RawLength == 0 {
		t.Fatalf("p1 block System = %+v, want p1's entry with its raw length", p.Blocks[1].System)
	}
	if p.Blocks[2].System == nil || p.Blocks[2].System.SeenAtRequest != "p1" {
		t.Fatalf("p2 block System = %+v, want the inherited p1 entry (same value, no row of its own)", p.Blocks[2].System)
	}
	if p.Blocks[3].System == nil || p.Blocks[3].System.SeenAtRequest != "p3" {
		t.Fatalf("p3 block System = %+v, want p3's own entry", p.Blocks[3].System)
	}

	fetchAt := func(query string) []transcriptSystemRaw {
		t.Helper()
		w, env := doJSON(t, r, http.MethodGet, "/api/admin/agent-sessions/"+sess+"/transcript/system?"+query, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("system?%s: status %d, body: %s", query, w.Code, w.Body.String())
		}
		var payload struct {
			System []transcriptSystemRaw `json:"system"`
		}
		if err := json.Unmarshal(env.Data, &payload); err != nil {
			t.Fatalf("system?%s: unmarshal: %v", query, err)
		}
		return payload.System
	}

	// No-param form stays the whole timeline (two deduplicated values).
	if got := fetchAt(""); len(got) != 2 {
		t.Fatalf("no-param timeline = %d entries, want 2", len(got))
	}
	// Per-request forms resolve to the effective value.
	if got := fetchAt("request=p1"); len(got) != 1 || got[0].Raw != "sys-one" || got[0].SeenAtRequest != "p1" {
		t.Fatalf("request=p1 = %+v, want its own entry", got)
	}
	if got := fetchAt("request=p2"); len(got) != 1 || got[0].Raw != "sys-one" || got[0].SeenAtRequest != "p1" {
		t.Fatalf("request=p2 = %+v, want the inherited p1 entry", got)
	}
	if got := fetchAt("request=p3"); len(got) != 1 || got[0].Raw != "sys-two" || got[0].SeenAtRequest != "p3" {
		t.Fatalf("request=p3 = %+v, want its own entry", got)
	}
	// A request with no value in effect, and an id that names no request of
	// the session: the lenient empty timeline (200, not an error).
	for _, q := range []string{"request=p0", "request=nope"} {
		if got := fetchAt(q); len(got) != 0 {
			t.Fatalf("system?%s = %+v, want the empty timeline", q, got)
		}
	}
	// Unknown session with the param: the domain 14006 envelope wins.
	w, env := doJSON(t, r, http.MethodGet, "/api/admin/agent-sessions/void/transcript/system?request=p1", nil, nil)
	if w.Code != http.StatusNotFound || env.Code != errcode.AgentSessionNotFound {
		t.Fatalf("unknown session with request param: status %d code %d, want 404/%d", w.Code, env.Code, errcode.AgentSessionNotFound)
	}
}

// TestGetAgentSessionTranscriptDomainErrorsWithParams pins TC-20's 14006
// arm: the new parameter shapes ride the same domain envelopes — an unknown
// session answers 14006 not-found on the transcript (with cursors), the
// transcript/system endpoint, and bad cursors on an unknown session stay
// 14006 (not-found wins over the lenient empty page). The member-403 arm is
// pinned by the router package's member-scope conformance test, which
// classifies both routes admin-only.
func TestGetAgentSessionTranscriptDomainErrorsWithParams(t *testing.T) {
	r, db := newAgentSessionTranscriptRouter(t, "")
	seedTranscriptPageSession(t, db)

	for _, path := range []string{
		"/api/admin/agent-sessions/nope/transcript?limit=5&before=req-x",
		"/api/admin/agent-sessions/nope/transcript?after=req-x",
		"/api/admin/agent-sessions/nope/transcript/system",
	} {
		w, env := doJSON(t, r, http.MethodGet, path, nil, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404, body: %s", path, w.Code, w.Body.String())
		}
		if env.Code != errcode.AgentSessionNotFound {
			t.Fatalf("%s: code = %d, want %d (AgentSessionNotFound)", path, env.Code, errcode.AgentSessionNotFound)
		}
	}
}
