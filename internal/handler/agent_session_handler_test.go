// Handler tests for the tool-session endpoints: the wire shape of the
// aggregate list (page envelope, JSON keys, hand-computed values), the
// agent_client query param, server-side pagination echoes, and the detail
// envelope including the 404 for an unknown session id. Same style as the
// request-log handler tests — a real service over a migrated SQLite DB,
// because the logic worth pinning is the SQL → service → handler → envelope
// chain.
package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/requestlog"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// newAgentSessionTestRouter wires the two tool-session routes over a fresh
// migrated SQLite DB with a real RequestLogService.
func newAgentSessionTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	svc := requestlog.NewRequestLogService(db)
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
