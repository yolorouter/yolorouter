package repository

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// stubSessionExtractor keys the session on the carrier contents the engine
// must hand it, so engine tests pin the selection, the cursor, the inputs,
// and the update guard without the real parsing rules in play — those have
// their own table-driven suite beside the extractor
// (internal/agentbackfill); the task-level tests below the engine run the
// real one end to end.
func stubSessionExtractor(agentClient string, body, headerSnapshot []byte) string {
	switch {
	case agentClient == "claude-code" && strings.Contains(string(body), "stub-body-session"):
		return "stub-session-from-body"
	case agentClient == "codex" && strings.Contains(string(headerSnapshot), "stub-header-session"):
		return "stub-session-from-headers"
	}
	return ""
}

// seedSessionRow inserts one request_logs row plus its request_log_bodies
// row carrying the given stored capture — the raw request body and/or the
// masked header snapshot, either of which may be "" for "not captured".
func seedSessionRow(t *testing.T, db *gorm.DB, requestID, body, headers string, mut func(*model.RequestLog)) {
	t.Helper()
	testutil.SeedRequestLog(t, db, requestID, time.Now().UTC(), mut)
	if err := UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:      requestID,
		RequestBody:    body,
		RequestHeaders: headers,
	}); err != nil {
		t.Fatalf("seed body row %s: %v", requestID, err)
	}
}

// readAgentSession reads one row's agent_session_id back as (value, isNull)
// — the NULL-versus-empty-string distinction these tests hinge on.
func readAgentSession(t *testing.T, db *gorm.DB, requestID string) (string, bool) {
	t.Helper()
	var row model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&row).Error; err != nil {
		t.Fatalf("read request_logs row %s: %v", requestID, err)
	}
	if row.AgentSessionID == nil {
		return "", true
	}
	return *row.AgentSessionID, false
}

func TestBackfillAgentSessionRoundAttributesOnlyTheTwoToolsNullSessions(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	claude, codex, opencode := "claude-code", "codex", "opencode"
	claudeBodyText := `{"metadata":{"user_id":"{\"session_id\":\"stub-body-session\"}"}}`
	codexSnapshotText := `{"User-Agent":["codex-tui/0.130.0"],"X-Codex-Turn-Metadata":["stub-header-session"]}`

	// A: claude-code, session NULL, body carries the session — the round's
	// first write, from the body path. B: codex, session NULL, snapshot
	// carries it and the body is EMPTY — the round's second write, from the
	// header path (the shape real codex history has). C: codex with a
	// capture the rules read no session from — scanned, left NULL. D:
	// codex with nothing captured at all — not even a candidate. E:
	// opencode, a session-carrying tool outside the two — never touched.
	// F: claude-code already carrying a session — outside the NULL set,
	// value must survive byte-identical. G: agent_client still NULL (the
	// client pass's residue) — no tool to dispatch on. H: claude-code with
	// no body row at all — nothing captured beside it to read.
	seedSessionRow(t, db, "sess-A", claudeBodyText, `{"User-Agent":["claude-cli/2.1.0"]}`, func(r *model.RequestLog) {
		r.AgentClient = &claude
	})
	seedSessionRow(t, db, "sess-B", "", codexSnapshotText, func(r *model.RequestLog) {
		r.AgentClient = &codex
	})
	seedSessionRow(t, db, "sess-C", `{"model":"gpt-5.2","input":[]}`, `{"User-Agent":["codex-tui/0.130.0"]}`, func(r *model.RequestLog) {
		r.AgentClient = &codex
	})
	seedSessionRow(t, db, "sess-D", "", "", func(r *model.RequestLog) {
		r.AgentClient = &codex
	})
	seedSessionRow(t, db, "sess-E", claudeBodyText, codexSnapshotText, func(r *model.RequestLog) {
		r.AgentClient = &opencode
	})
	existing := "live-session-keep"
	seedSessionRow(t, db, "sess-F", claudeBodyText, "", func(r *model.RequestLog) {
		r.AgentClient = &claude
		r.AgentSessionID = &existing
	})
	seedSessionRow(t, db, "sess-G", claudeBodyText, "", nil)
	seedBackfillRow(t, db, "sess-H", nil, func(r *model.RequestLog) {
		r.AgentClient = &claude
	})

	lastID, scanned, updated, err := BackfillAgentSessionRound(db, 0, 10, stubSessionExtractor)
	if err != nil {
		t.Fatalf("BackfillAgentSessionRound: %v", err)
	}
	if scanned != 3 || updated != 2 {
		t.Fatalf("round counts: scanned=%d updated=%d, want scanned=3 (A,B,C) updated=2 (A,B)", scanned, updated)
	}
	if lastID != rowID(t, db, "sess-C") {
		t.Fatalf("lastID=%d, want the highest scanned row's id (C's %d)", lastID, rowID(t, db, "sess-C"))
	}

	for _, tc := range []struct {
		requestID string
		want      string
		wantNull  bool
	}{
		{"sess-A", "stub-session-from-body", false},
		{"sess-B", "stub-session-from-headers", false},
		{"sess-C", "", true},
		{"sess-D", "", true},
		{"sess-E", "", true},
		{"sess-F", "live-session-keep", false},
		{"sess-G", "", true},
		{"sess-H", "", true},
	} {
		got, isNull := readAgentSession(t, db, tc.requestID)
		switch {
		case tc.wantNull && !isNull:
			t.Fatalf("%s: agent_session_id=%q, want SQL NULL — never the empty string", tc.requestID, got)
		case !tc.wantNull && got != tc.want:
			t.Fatalf("%s: agent_session_id=%q, want %q", tc.requestID, got, tc.want)
		}
	}
}

func TestBackfillAgentSessionRoundWalksTheCursorInBatches(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	// Five attributable rows; batches of two must walk them oldest-first
	// under the cursor without missing or rescanning any.
	codex := "codex"
	snapshot := `{"X-Codex-Turn-Metadata":["stub-header-session"]}`
	for _, id := range []string{"swim-1", "swim-2", "swim-3", "swim-4", "swim-5"} {
		seedSessionRow(t, db, id, "", snapshot, func(r *model.RequestLog) {
			r.AgentClient = &codex
		})
	}

	var totalUpdated int
	var cursor uint
	for round := 1; ; round++ {
		if round > 5 {
			t.Fatal("pass did not terminate: cursor is not advancing")
		}
		lastID, scanned, updated, err := BackfillAgentSessionRound(db, cursor, 2, stubSessionExtractor)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if lastID <= cursor && scanned > 0 {
			t.Fatalf("round %d: cursor went backwards: %d after %d", round, lastID, cursor)
		}
		cursor = lastID
		totalUpdated += updated
		if scanned < 2 {
			break
		}
	}
	if totalUpdated != 5 {
		t.Fatalf("total updated=%d, want 5", totalUpdated)
	}
	var nullCount int64
	if err := db.Model(&model.RequestLog{}).Where("agent_session_id IS NULL").Count(&nullCount).Error; err != nil {
		t.Fatalf("count NULL agent_session_id: %v", err)
	}
	if nullCount != 0 {
		t.Fatalf("rows left NULL: %d, want 0", nullCount)
	}

	// A pass over a drained table is one empty round: nothing scanned,
	// nothing updated, no error.
	_, scanned, updated, err := BackfillAgentSessionRound(db, 0, 2, stubSessionExtractor)
	if err != nil {
		t.Fatalf("converged round: %v", err)
	}
	if scanned != 0 || updated != 0 {
		t.Fatalf("converged round: scanned=%d updated=%d, want 0/0", scanned, updated)
	}
}

func TestBackfillAgentSessionRoundRerunChangesNothing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	claude, codex := "claude-code", "codex"
	seedSessionRow(t, db, "rerun-A", `{"metadata":{"user_id":"{\"session_id\":\"stub-body-session\"}"}}`, "", func(r *model.RequestLog) {
		r.AgentClient = &claude
	})
	seedSessionRow(t, db, "rerun-C", "", `{"User-Agent":["codex-tui/0.130.0"]}`, func(r *model.RequestLog) {
		r.AgentClient = &codex
	})

	// Drain the table once, then snapshot every row whole: a second pass
	// over it must not exist as far as the data is concerned — not a new
	// value, not '' where NULL was, not a touched column, nothing.
	drain := func() (totalUpdated int) {
		var cursor uint
		for {
			lastID, scanned, updated, err := BackfillAgentSessionRound(db, cursor, 10, stubSessionExtractor)
			if err != nil {
				t.Fatalf("round: %v", err)
			}
			cursor = lastID
			totalUpdated += updated
			if scanned < 10 {
				return totalUpdated
			}
		}
	}
	if n := drain(); n != 1 {
		t.Fatalf("first drain updated=%d, want 1 (rerun-A)", n)
	}
	before := map[string]model.RequestLog{}
	for _, requestID := range []string{"rerun-A", "rerun-C"} {
		var row model.RequestLog
		if err := db.Where("request_id = ?", requestID).First(&row).Error; err != nil {
			t.Fatalf("read %s: %v", requestID, err)
		}
		before[requestID] = row
	}

	if n := drain(); n != 0 {
		t.Fatalf("second drain updated=%d, want 0 — the pass must be idempotent", n)
	}
	for requestID, want := range before {
		var got model.RequestLog
		if err := db.Where("request_id = ?", requestID).First(&got).Error; err != nil {
			t.Fatalf("reread %s: %v", requestID, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed on rerun: before=%+v after=%+v", requestID, want, got)
		}
	}
}

func TestBackfillAgentSessionRoundUpdateYieldsToARacingWriter(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	claude := "claude-code"
	seedSessionRow(t, db, "race-sess", `{"metadata":{"user_id":"{\"session_id\":\"stub-body-session\"}"}}`, "", func(r *model.RequestLog) {
		r.AgentClient = &claude
	})

	// Simulate the race the NULL guard exists for: while the round holds
	// the selected row, another writer fills its session first. The
	// extractor runs inside the round's update window, so giving it the
	// side effect lands the racing write exactly between SELECT and UPDATE.
	racing := func(agentClient string, body, headerSnapshot []byte) string {
		if !strings.Contains(string(body), "stub-body-session") {
			return ""
		}
		if err := db.Model(&model.RequestLog{}).
			Where("request_id = ?", "race-sess").
			UpdateColumn("agent_session_id", "racing-writer-session").Error; err != nil {
			t.Fatalf("racing writer update: %v", err)
		}
		return "stub-session-from-body"
	}

	_, _, updated, err := BackfillAgentSessionRound(db, 0, 10, racing)
	if err != nil {
		t.Fatalf("BackfillAgentSessionRound: %v", err)
	}
	if updated != 0 {
		t.Fatalf("updated=%d, want 0 — the guard must let the racing writer win", updated)
	}
	got, isNull := readAgentSession(t, db, "race-sess")
	if isNull || got != "racing-writer-session" {
		t.Fatalf("agent_session_id=%q null=%v, want the racing writer's %q untouched", got, isNull, "racing-writer-session")
	}
}

// TestBackfilledRowsGroupCleanlyUnderTheSessionQueries is the
// backfill/view interaction regression lock: rows this engine just wrote
// must flow through the existing session queries (zero query changes) as
// ordinary sessions — grouped by their backfilled id, attributed to the
// first row's tool — while rows that stayed NULL form no group at all and
// no empty-string pseudo-session ever appears.
func TestBackfilledRowsGroupCleanlyUnderTheSessionQueries(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	claude, codex := "claude-code", "codex"
	claudeBodyText := `{"metadata":{"user_id":"{\"session_id\":\"stub-body-session\"}"}}`
	codexSnapshotText := `{"X-Codex-Turn-Metadata":["stub-header-session"]}`

	seedTool := func(requestID string, offset time.Duration, client string, body, headers string) {
		t.Helper()
		seedSessionRow(t, db, requestID, body, headers, func(r *model.RequestLog) {
			r.AgentClient = &client
			r.CreatedAt = base.Add(offset)
		})
	}

	// Two claude-code rows sharing one body-carried session, plus one codex
	// row with its own header-carried session — the backfilled groups. And
	// one codex row with no carrier: it stays NULL and must not group.
	seedTool("grp-a1", 0, claude, claudeBodyText, "")
	seedTool("grp-a2", 5*time.Minute, claude, claudeBodyText, "")
	seedTool("grp-b1", 10*time.Minute, codex, "", codexSnapshotText)
	seedTool("grp-null", 15*time.Minute, codex, "", "")

	var cursor uint
	for {
		lastID, scanned, _, err := BackfillAgentSessionRound(db, cursor, 2, stubSessionExtractor)
		if err != nil {
			t.Fatalf("round: %v", err)
		}
		cursor = lastID
		if scanned < 2 {
			break
		}
	}

	sessions, total, err := ListAgentSessions(db, &AgentSessionFilter{})
	if err != nil {
		t.Fatalf("ListAgentSessions: %v", err)
	}
	if total != 2 {
		t.Fatalf("total sessions=%d, want 2 — NULL rows must not group", total)
	}
	byID := map[string]AgentSessionAggregate{}
	for _, s := range sessions {
		if s.AgentSessionID == "" {
			t.Fatalf("empty-string session id listed: %+v — the backfill must never write ''", s)
		}
		byID[s.AgentSessionID] = s
	}
	a, ok := byID["stub-session-from-body"]
	if !ok {
		t.Fatalf("backfilled claude session missing from list: %+v", sessions)
	}
	if a.RequestCount != 2 || a.AgentClient != "claude-code" {
		t.Fatalf("claude group: request_count=%d agent_client=%q, want 2/claude-code", a.RequestCount, a.AgentClient)
	}
	if a.InputTokens != 20 || a.OutputTokens != 40 {
		t.Fatalf("claude group tokens: in=%d out=%d, want 20/40 (both backfilled rows aggregated)", a.InputTokens, a.OutputTokens)
	}
	b, ok := byID["stub-session-from-headers"]
	if !ok {
		t.Fatalf("backfilled codex session missing from list: %+v", sessions)
	}
	if b.RequestCount != 1 || b.AgentClient != "codex" {
		t.Fatalf("codex group: request_count=%d agent_client=%q, want 1/codex", b.RequestCount, b.AgentClient)
	}

	if n, err := CountAgentSessions(db, &AgentSessionFilter{}); err != nil || n != 2 {
		t.Fatalf("CountAgentSessions()=%d err=%v, want 2/nil", n, err)
	}
	claudeFilter := "claude-code"
	if n, err := CountAgentSessions(db, &AgentSessionFilter{AgentClient: &claudeFilter}); err != nil || n != 1 {
		t.Fatalf("CountAgentSessions(claude-code)=%d err=%v, want 1/nil", n, err)
	}
}
