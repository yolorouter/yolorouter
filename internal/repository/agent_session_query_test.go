// Data-layer tests for the tool-session aggregate view (repository/
// agent_session_query.go), against a real migrated SQLite database.
//
// One seed matrix (seedAgentSessionMatrix) drives the list and detail
// surfaces: session A with rows across all five status classes, mixed
// token/cost shapes (including a cost_known=false row whose cost_micros is
// deliberately NON-zero, to pin that the known-cost sum is guarded by
// cost_known rather than by "finalize zeroes the column"), session B with a
// same-timestamp row pair (id tie-break), one header-less row (a recognized
// tool that sends no session header), and one same-session-different-tool
// anomaly row inside A. Every expected aggregate below is hand-computed
// from the table in seedAgentSessionMatrix's doc comment — when a number
// here disagrees with the SQL, the test is right to fail.
package repository

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedAgentSessionMatrix seeds the canonical test world (see the package
// doc above for the full table and the hand-computed aggregates):
//
//	session A "sess-a" (attribution claude-code — the first row wins; the
//	codex anomaly row a7 is deliberately NOT first)
//	session B "sess-b" (attribution codex; b2/b3 share a timestamp so the
//	chronological order falls to the id tie-break)
//	one header-less row (recognized tool gemini-cli, no session header)
//
// The insert order below IS the id order, which the detail tie-break and
// the "first row" attribution depend on.
func seedAgentSessionMatrix(t *testing.T, db *gorm.DB) time.Time {
	t.Helper()
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	claude := "claude-code"
	codex := "codex"
	gemini := "gemini-cli"
	truncated := "stream truncated"

	// Session A: all five status classes, one unknown-cost row with a
	// non-zero cost_micros (a5), and the mixed-tool anomaly (a7, codex).
	seed := func(requestID string, offset time.Duration, client string, mut func(*model.RequestLog)) {
		t.Helper()
		testutil.SeedRequestLog(t, db, requestID, base.Add(offset), func(r *model.RequestLog) {
			r.AgentClient = &client
			sess := "sess-a"
			r.AgentSessionID = &sess
			mut(r)
		})
	}
	seed("req-a1", 0, claude, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 100, 200, 1500, true
	})
	seed("req-a2", 5*time.Minute, claude, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 500, 10, 0, 0, true
	})
	seed("req-a3", 10*time.Minute, claude, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 20, 30, 70, true
		r.FailReason = &truncated // 2xx WITH a fail reason = partial
	})
	seed("req-a4", 15*time.Minute, claude, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 499, 0, 0, 0, true
	})
	seed("req-a5", 20*time.Minute, claude, func(r *model.RequestLog) {
		// Unknown-cost row carrying a non-zero micros value: the known-cost
		// sum must exclude it by cost_known, not by trusting the column to
		// hold zero.
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 429, 5, 0, 999, false
	})
	seed("req-a6", 25*time.Minute, claude, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 50, 60, 80, true
	})
	// The anomaly: same session id, a DIFFERENT tool, not the first row.
	seed("req-a7", 30*time.Minute, codex, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 7, 8, 10, true
	})

	// Session B: newer last activity than A (drives the default order), and
	// b2/b3 share a timestamp so only the id order separates them.
	seedB := func(requestID string, offset time.Duration, mut func(*model.RequestLog)) {
		t.Helper()
		testutil.SeedRequestLog(t, db, requestID, base.Add(offset), func(r *model.RequestLog) {
			r.AgentClient = &codex
			sess := "sess-b"
			r.AgentSessionID = &sess
			mut(r)
		})
	}
	seedB("req-b1", 2*time.Hour, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 1000, 2000, 5000, true
	})
	seedB("req-b2", 2*time.Hour+5*time.Minute, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 404, 10, 0, 0, true
	})
	seedB("req-b3", 2*time.Hour+5*time.Minute, func(r *model.RequestLog) {
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 1, 2, 3, true
	})

	// Header-less row: a recognized tool that sends no session header. Its
	// created_at is the NEWEST of all rows, so it would top the list if the
	// NULL-session exclusion or the ordering were wrong.
	testutil.SeedRequestLog(t, db, "req-none", base.Add(3*time.Hour), func(r *model.RequestLog) {
		r.AgentClient = &gemini
		r.AgentSessionID = nil
		r.StatusCode, r.InputTokens, r.OutputTokens, r.CostMicros, r.CostKnown = 200, 500, 500, 100, true
	})
	return base
}

// sessionIDs extracts the ordered session-id column from a result page.
func sessionIDs(sessions []AgentSessionAggregate) []string {
	ids := make([]string, 0, len(sessions))
	for i := range sessions {
		ids = append(ids, sessions[i].AgentSessionID)
	}
	return ids
}

// TestListAgentSessionsAggregatesPerSession pins the per-session aggregate
// values against the hand-computed matrix: exactly the two real sessions
// appear (the header-less row never forms one), every field — request
// count, success count, token sums, known/unknown cost split, first/last
// seen — equals its hand value, attribution is the FIRST row's tool despite
// the mixed-tool anomaly row, and the default order is most-recent-activity
// first.
func TestListAgentSessionsAggregatesPerSession(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := seedAgentSessionMatrix(t, db)

	sessions, total, err := ListAgentSessions(db, &AgentSessionFilter{})
	if err != nil {
		t.Fatalf("ListAgentSessions: %v", err)
	}
	if total != 2 || len(sessions) != 2 {
		t.Fatalf("expected exactly the 2 seeded sessions, got total=%d rows=%v", total, sessionIDs(sessions))
	}
	// B's last activity (+2h5m) is newer than A's (+30m): B leads.
	if got := sessionIDs(sessions); got[0] != "sess-b" || got[1] != "sess-a" {
		t.Fatalf("default order = %v, want [sess-b sess-a] (most recent activity first)", got)
	}

	a := sessions[1]
	if a.AgentSessionID != "sess-a" {
		t.Fatalf("second row = %s, want sess-a", a.AgentSessionID)
	}
	// Hand-computed expectations for session A, one assertion per field so
	// a failure names the exact drifted aggregate.
	if a.AgentClient != "claude-code" {
		t.Errorf("A attribution = %q, want claude-code (first row's tool; the codex anomaly row must not win)", a.AgentClient)
	}
	if a.RequestCount != 7 {
		t.Errorf("A request_count = %d, want 7 (all rows incl. the mixed-tool anomaly)", a.RequestCount)
	}
	if a.SuccessCount != 3 {
		t.Errorf("A success_count = %d, want 3 (a1, a6, a7 — the success class only)", a.SuccessCount)
	}
	if a.InputTokens != 192 {
		t.Errorf("A input_tokens = %d, want 192 (100+10+20+0+5+50+7)", a.InputTokens)
	}
	if a.OutputTokens != 298 {
		t.Errorf("A output_tokens = %d, want 298 (200+0+30+0+0+60+8)", a.OutputTokens)
	}
	if a.KnownCostMicros != 1660 {
		t.Errorf("A known_cost_micros = %d, want 1660 (1500+70+80+10; the unknown row's 999 excluded)", a.KnownCostMicros)
	}
	if a.UnknownCostCount != 1 {
		t.Errorf("A unknown_cost_count = %d, want 1 (a5)", a.UnknownCostCount)
	}
	if want := base.Add(30 * time.Minute); !a.LastSeenAt.Equal(want) {
		t.Errorf("A last_seen_at = %v, want %v", a.LastSeenAt, want)
	}

	b := sessions[0]
	if b.AgentSessionID != "sess-b" {
		t.Fatalf("first row = %s, want sess-b", b.AgentSessionID)
	}
	if b.AgentClient != "codex" {
		t.Errorf("B attribution = %q, want codex", b.AgentClient)
	}
	if b.RequestCount != 3 {
		t.Errorf("B request_count = %d, want 3", b.RequestCount)
	}
	if b.SuccessCount != 2 {
		t.Errorf("B success_count = %d, want 2 (b1, b3)", b.SuccessCount)
	}
	if b.InputTokens != 1011 {
		t.Errorf("B input_tokens = %d, want 1011 (1000+10+1)", b.InputTokens)
	}
	if b.OutputTokens != 2002 {
		t.Errorf("B output_tokens = %d, want 2002 (2000+0+2)", b.OutputTokens)
	}
	if b.KnownCostMicros != 5003 {
		t.Errorf("B known_cost_micros = %d, want 5003 (5000+0+3)", b.KnownCostMicros)
	}
	if b.UnknownCostCount != 0 {
		t.Errorf("B unknown_cost_count = %d, want 0", b.UnknownCostCount)
	}
	if want := base.Add(2 * time.Hour); !b.FirstSeenAt.Equal(want) {
		t.Errorf("B first_seen_at = %v, want %v", b.FirstSeenAt, want)
	}
	if want := base.Add(2*time.Hour + 5*time.Minute); !b.LastSeenAt.Equal(want) {
		t.Errorf("B last_seen_at = %v, want %v", b.LastSeenAt, want)
	}
}

// TestListAgentSessionsFiltersByAttribution pins the agent_client filter's
// semantics: it selects sessions by ATTRIBUTION (the first row's tool) while
// a session's aggregates keep covering all of its rows. Session A contains
// a codex row but is attributed claude-code, so the codex filter must return
// only B — and the header-less gemini-cli row must never surface, because
// it forms no session at all. A present-but-empty filter value is a real
// constraint matching nothing; nil is filter off.
func TestListAgentSessionsFiltersByAttribution(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAgentSessionMatrix(t, db)

	list := func(client *string) ([]string, int64) {
		t.Helper()
		sessions, total, err := ListAgentSessions(db, &AgentSessionFilter{AgentClient: client})
		if err != nil {
			t.Fatalf("ListAgentSessions(%v): %v", client, err)
		}
		return sessionIDs(sessions), total
	}

	if ids, total := list(testutil.Ptr("claude-code")); total != 1 || len(ids) != 1 || ids[0] != "sess-a" {
		t.Fatalf("filter claude-code = (%v, total=%d), want exactly [sess-a]", ids, total)
	}
	// The attribution arm: A CONTAINS a codex row, yet is not a codex
	// session — only B answers the codex filter.
	if ids, total := list(testutil.Ptr("codex")); total != 1 || len(ids) != 1 || ids[0] != "sess-b" {
		t.Fatalf("filter codex = (%v, total=%d), want exactly [sess-b] — a session containing a codex row but attributed claude-code must not match", ids, total)
	}
	// A recognized-but-header-less tool forms no session, so its filter
	// yields nothing.
	if ids, total := list(testutil.Ptr("gemini-cli")); total != 0 || len(ids) != 0 {
		t.Fatalf("filter gemini-cli = (%v, total=%d), want no sessions", ids, total)
	}
	// Present-but-empty is a real constraint (attribution is never ""), not
	// a silent "filter off".
	if ids, total := list(testutil.Ptr("")); total != 0 || len(ids) != 0 {
		t.Fatalf("filter \"\" = (%v, total=%d), want no sessions", ids, total)
	}
	// nil = filter off: both sessions.
	if ids, total := list(nil); total != 2 || len(ids) != 2 {
		t.Fatalf("no filter = (%v, total=%d), want both sessions", ids, total)
	}
}

// TestListAgentSessionsPaginatesServerSide pins the pagination contract:
// LIMIT/OFFSET pages over the ordered session set with the total counted
// across the whole filter, and the Page/PageSize defaults/clamps mirror
// ListRequestLogs (page 1, size 20).
func TestListAgentSessionsPaginatesServerSide(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAgentSessionMatrix(t, db)

	page := func(p, size int) ([]string, int64) {
		t.Helper()
		sessions, total, err := ListAgentSessions(db, &AgentSessionFilter{Page: p, PageSize: size})
		if err != nil {
			t.Fatalf("ListAgentSessions(page=%d,size=%d): %v", p, size, err)
		}
		return sessionIDs(sessions), total
	}

	if ids, total := page(1, 1); total != 2 || len(ids) != 1 || ids[0] != "sess-b" {
		t.Fatalf("page 1 size 1 = (%v, total=%d), want [sess-b] total=2", ids, total)
	}
	if ids, total := page(2, 1); total != 2 || len(ids) != 1 || ids[0] != "sess-a" {
		t.Fatalf("page 2 size 1 = (%v, total=%d), want [sess-a] total=2", ids, total)
	}
	if ids, total := page(3, 1); total != 2 || len(ids) != 0 {
		t.Fatalf("page 3 size 1 = (%v, total=%d), want no rows, total=2", ids, total)
	}
	// Zero values fall back to the default page 1 / size 20.
	if ids, total := page(0, 0); total != 2 || len(ids) != 2 {
		t.Fatalf("page 0 size 0 = (%v, total=%d), want both sessions (defaults)", ids, total)
	}
}

// TestListAgentSessionsOrdersTieBySessionID pins the deterministic
// tie-break: sessions whose LAST activity is identical order by
// agent_session_id ascending, still behind any session with newer activity.
func TestListAgentSessionsOrdersTieBySessionID(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	seed := func(sessionID string, offsets ...time.Duration) {
		t.Helper()
		for _, off := range offsets {
			sess := sessionID
			testutil.SeedRequestLog(t, db, "req-"+sessionID+"-"+off.String(), base.Add(off), func(r *model.RequestLog) {
				r.AgentClient = &sess
				r.AgentSessionID = &sess
			})
		}
	}
	// c and e share the newest last activity (+2h): the tie-break orders
	// them by session id ascending; d's last activity (+1h) is older, so it
	// trails both regardless of its earlier start.
	seed("sess-c", time.Hour, 2*time.Hour)
	seed("sess-d", 0, time.Hour)
	seed("sess-e", 90*time.Minute, 2*time.Hour)

	sessions, total, err := ListAgentSessions(db, &AgentSessionFilter{})
	if err != nil {
		t.Fatalf("ListAgentSessions: %v", err)
	}
	if total != 3 || len(sessions) != 3 {
		t.Fatalf("expected 3 sessions, got total=%d rows=%v", total, sessionIDs(sessions))
	}
	if got := sessionIDs(sessions); got[0] != "sess-c" || got[1] != "sess-e" || got[2] != "sess-d" {
		t.Fatalf("order = %v, want [sess-c sess-e sess-d] (activity DESC, id ASC tie-break)", got)
	}
}

// TestListRequestLogsByAgentSessionChronological pins the session detail
// query: every row of the session in (created_at, id) order — including the
// mixed-tool anomaly row — with no row leaking in from another session or
// from the header-less traffic, and nothing at all for an unknown id.
func TestListRequestLogsByAgentSessionChronological(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	seedAgentSessionMatrix(t, db)

	requestIDs := func(rows []model.RequestLog) []string {
		ids := make([]string, 0, len(rows))
		for i := range rows {
			ids = append(ids, rows[i].RequestID)
		}
		return ids
	}

	rows, err := ListRequestLogsByAgentSession(db, "sess-a")
	if err != nil {
		t.Fatalf("ListRequestLogsByAgentSession(sess-a): %v", err)
	}
	want := []string{"req-a1", "req-a2", "req-a3", "req-a4", "req-a5", "req-a6", "req-a7"}
	if got := requestIDs(rows); !equalStrings(got, want) {
		t.Fatalf("sess-a timeline = %v, want %v (chronological, anomaly row included)", got, want)
	}
	for i := range rows {
		if rows[i].AgentSessionID == nil || *rows[i].AgentSessionID != "sess-a" {
			t.Fatalf("row %s leaked into sess-a with session %v", rows[i].RequestID, rows[i].AgentSessionID)
		}
	}

	// b2/b3 share created_at: the id tie-break must keep the insert order.
	rowsB, err := ListRequestLogsByAgentSession(db, "sess-b")
	if err != nil {
		t.Fatalf("ListRequestLogsByAgentSession(sess-b): %v", err)
	}
	if got := requestIDs(rowsB); !equalStrings(got, []string{"req-b1", "req-b2", "req-b3"}) {
		t.Fatalf("sess-b timeline = %v, want [req-b1 req-b2 req-b3] (id tie-break on equal timestamps)", got)
	}

	// An unknown session yields an empty result (the service maps that to
	// its not-found error).
	rowsX, err := ListRequestLogsByAgentSession(db, "sess-nope")
	if err != nil {
		t.Fatalf("ListRequestLogsByAgentSession(sess-nope): %v", err)
	}
	if len(rowsX) != 0 {
		t.Fatalf("unknown session returned %d rows, want 0", len(rowsX))
	}
}

// equalStrings is a tiny slice equality helper, local so the test file
// carries no dependency on a generic assert package.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
