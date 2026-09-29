// Service-layer tests for the tool-session view (agent_session_service.go):
// the detail payload's five-class status per row, the first-row attribution
// flattening, the not-found mapping for an unknown or empty session id, and
// the list DTO conversion — over a real migrated SQLite DB, mirroring the
// repository matrix's session A so the two layers assert the same world.
package requestlog

import (
	"errors"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// seedSessionA seeds a compact session A — five status classes plus the
// mixed-tool anomaly row, same shape as the repository matrix — and returns
// the service bound to the seeded DB.
func seedSessionA(t *testing.T) (*RequestLogService, time.Time) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db)
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	claude := "claude-code"
	codex := "codex"
	truncated := "stream truncated"

	rows := []struct {
		requestID string
		offset    time.Duration
		client    string
		mut       func(*model.RequestLog)
	}{
		{"req-a1", 0, claude, func(r *model.RequestLog) {
			r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 100, 200, 1500, true
		}},
		{"req-a2", 5 * time.Minute, claude, func(r *model.RequestLog) {
			r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 500, 10, 0, 0, true
		}},
		{"req-a3", 10 * time.Minute, claude, func(r *model.RequestLog) {
			r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 20, 30, 70, true
			r.FailReason = &truncated
		}},
		{"req-a4", 15 * time.Minute, claude, func(r *model.RequestLog) {
			r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 499, 0, 0, 0, true
		}},
		{"req-a5", 20 * time.Minute, claude, func(r *model.RequestLog) {
			r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 429, 5, 0, 999, false
		}},
		{"req-a6", 25 * time.Minute, claude, func(r *model.RequestLog) {
			r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 50, 60, 80, true
		}},
		// The anomaly: a later codex row inside the claude-code session.
		{"req-a7", 30 * time.Minute, codex, func(r *model.RequestLog) {
			r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 7, 8, 10, true
		}},
	}
	for _, row := range rows {
		client := row.client
		testutil.SeedRequestLog(t, db, row.requestID, base.Add(row.offset), func(r *model.RequestLog) {
			r.AgentClient = &client
			sess := "sess-a"
			r.AgentSessionID = &sess
			row.mut(r)
		})
	}
	return svc, base
}

// TestGetAgentSessionDetailCarriesFiveClassStatus pins the detail payload:
// all seven rows in chronological order, each carrying its five-class
// status_class (the same DeriveStatusClass the request-log list uses), the
// mixed-tool anomaly row included, and the attribution flattened from the
// FIRST row's nullable column.
func TestGetAgentSessionDetailCarriesFiveClassStatus(t *testing.T) {
	svc, base := seedSessionA(t)

	detail, err := svc.GetAgentSessionDetail("sess-a")
	if err != nil {
		t.Fatalf("GetAgentSessionDetail(sess-a): %v", err)
	}
	if detail.AgentSessionID != "sess-a" {
		t.Errorf("detail agent_session_id = %q, want sess-a", detail.AgentSessionID)
	}
	if detail.AgentClient != "claude-code" {
		t.Errorf("detail attribution = %q, want claude-code (first row's tool, anomaly row loses)", detail.AgentClient)
	}
	if len(detail.Requests) != 7 {
		t.Fatalf("detail requests = %d rows, want 7 (the anomaly row included)", len(detail.Requests))
	}
	wantClasses := []string{
		repository.StatusSuccess,   // a1 200 no fail
		repository.StatusFailed,    // a2 500
		repository.StatusPartial,   // a3 200 with fail_reason
		repository.StatusCancelled, // a4 499
		repository.StatusRejected,  // a5 429
		repository.StatusSuccess,   // a6 200 no fail
		repository.StatusSuccess,   // a7 200 no fail (the codex anomaly row)
	}
	for i, want := range wantClasses {
		got := detail.Requests[i]
		if got.StatusClass != want {
			t.Errorf("row %d (%s) status_class = %q, want %q", i, got.RequestID, got.StatusClass, want)
		}
	}
	// Chronological: the timeline reads earliest → latest.
	if got := detail.Requests[0].RequestID; got != "req-a1" {
		t.Errorf("first timeline row = %s, want req-a1 (chronological order)", got)
	}
	if got := detail.Requests[6].RequestID; got != "req-a7" {
		t.Errorf("last timeline row = %s, want req-a7 (chronological order)", got)
	}
	if want := base.Add(30 * time.Minute); !detail.Requests[6].CreatedAt.Equal(want) {
		t.Errorf("last timeline row created_at = %v, want %v", detail.Requests[6].CreatedAt, want)
	}
}

// TestGetAgentSessionDetailNotFound pins the not-found mapping: an unknown
// session id and an empty one both answer errcode.ErrAgentSessionNotFound
// (the handler renders that as the 404 envelope).
func TestGetAgentSessionDetailNotFound(t *testing.T) {
	svc, _ := seedSessionA(t)

	for _, id := range []string{"sess-nope", ""} {
		if _, err := svc.GetAgentSessionDetail(id); !errors.Is(err, errcode.ErrAgentSessionNotFound) {
			t.Errorf("GetAgentSessionDetail(%q) error = %v, want ErrAgentSessionNotFound", id, err)
		}
	}
}

// TestListAgentSessionsMapsAggregateRows pins the list DTO conversion over
// the same session A: every repository aggregate lands on the wire row
// verbatim (hand-computed values from the matrix) and the nullable
// attribution arrives flattened as a plain string.
func TestListAgentSessionsMapsAggregateRows(t *testing.T) {
	svc, base := seedSessionA(t)

	items, total, err := svc.ListAgentSessions(&repository.AgentSessionFilter{})
	if err != nil {
		t.Fatalf("ListAgentSessions: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("expected exactly sess-a, got total=%d items=%d", total, len(items))
	}
	a := items[0]
	if a.AgentSessionID != "sess-a" || a.AgentClient != "claude-code" {
		t.Fatalf("list row = (%s, %q), want (sess-a, claude-code)", a.AgentSessionID, a.AgentClient)
	}
	if a.RequestCount != 7 || a.SuccessCount != 3 {
		t.Errorf("list row counts = (%d requests, %d success), want (7, 3)", a.RequestCount, a.SuccessCount)
	}
	if a.InputTokens != 192 || a.OutputTokens != 298 {
		t.Errorf("list row tokens = (%d in, %d out), want (192, 298)", a.InputTokens, a.OutputTokens)
	}
	if a.KnownCostMicros != 1660 || a.UnknownCostCount != 1 {
		t.Errorf("list row cost = (%d known micros, %d unknown rows), want (1660, 1)", a.KnownCostMicros, a.UnknownCostCount)
	}
	if want := base; !a.FirstSeenAt.Equal(want) {
		t.Errorf("list row first_seen_at = %v, want %v", a.FirstSeenAt, want)
	}
	if want := base.Add(30 * time.Minute); !a.LastSeenAt.Equal(want) {
		t.Errorf("list row last_seen_at = %v, want %v", a.LastSeenAt, want)
	}
}
