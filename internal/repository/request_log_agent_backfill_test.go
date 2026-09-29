package repository

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// stubRecognizer names the tool recorded as the snapshot's whole content, so
// engine tests pin the selection, the cursor, and the update guard without
// the real parsing rules in play (those have their own table-driven suite
// next to the recognizer; the task-level test below the engine runs the
// real one end to end).
func stubRecognizer(snapshot []byte) string {
	if string(snapshot) == "stub-signature" {
		return "stub-tool"
	}
	return ""
}

// seedBackfillRow inserts one request_logs row plus (unless headers is nil)
// its request_log_bodies row carrying the given stored header snapshot —
// the exact shape the gateway leaves behind. A nil headers seeds a request
// with no body row at all; an empty string seeds a body row whose header
// capture is missing.
func seedBackfillRow(t *testing.T, db *gorm.DB, requestID string, headers *string, mut func(*model.RequestLog)) {
	t.Helper()
	testutil.SeedRequestLog(t, db, requestID, time.Now().UTC(), mut)
	if headers == nil {
		return
	}
	if err := UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:      requestID,
		RequestHeaders: *headers,
	}); err != nil {
		t.Fatalf("seed body row %s: %v", requestID, err)
	}
}

// readAgentClient reads one row's agent_client back as (value, isNull).
func readAgentClient(t *testing.T, db *gorm.DB, requestID string) (string, bool) {
	t.Helper()
	var row model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&row).Error; err != nil {
		t.Fatalf("read request_logs row %s: %v", requestID, err)
	}
	if row.AgentClient == nil {
		return "", true
	}
	return *row.AgentClient, false
}

// rowID returns the row's primary key, for cursor assertions.
func rowID(t *testing.T, db *gorm.DB, requestID string) uint {
	t.Helper()
	var row model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&row).Error; err != nil {
		t.Fatalf("read request_logs row %s: %v", requestID, err)
	}
	return row.ID
}

func TestBackfillAgentClientRoundAttributesOnlyNullRowsWithStoredSnapshot(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	stubHeaders := "stub-signature"
	toolUser := "irrelevant"
	// A names the tool (NULL client + stored signature) — the round's only
	// write. B has a snapshot the recognizer cannot name — scanned, left
	// NULL. C has a body row with no header capture — not a candidate. D
	// has no body row at all — not a candidate. E is already attributed —
	// outside the NULL set, and its value must survive byte-identical.
	seedBackfillRow(t, db, "req-A", &stubHeaders, nil)
	seedBackfillRow(t, db, "req-B", &toolUser, nil)
	seedBackfillRow(t, db, "req-C", new(string), nil)
	seedBackfillRow(t, db, "req-D", nil, nil)
	seedBackfillRow(t, db, "req-E", &stubHeaders, func(row *model.RequestLog) {
		row.AgentClient = testutil.Ptr("codex")
	})

	lastID, scanned, updated, err := BackfillAgentClientRound(db, 0, 10, stubRecognizer)
	if err != nil {
		t.Fatalf("BackfillAgentClientRound: %v", err)
	}
	if scanned != 2 || updated != 1 {
		t.Fatalf("round counts: scanned=%d updated=%d, want scanned=2 (A,B) updated=1 (A)", scanned, updated)
	}
	if lastID != rowID(t, db, "req-B") {
		t.Fatalf("lastID=%d, want the highest scanned row's id (B's %d)", lastID, rowID(t, db, "req-B"))
	}

	for _, tc := range []struct {
		requestID string
		want      string
		wantNull  bool
	}{
		{"req-A", "stub-tool", false},
		{"req-B", "", true},
		{"req-C", "", true},
		{"req-D", "", true},
		{"req-E", "codex", false},
	} {
		got, isNull := readAgentClient(t, db, tc.requestID)
		switch {
		case tc.wantNull && !isNull:
			t.Fatalf("%s: agent_client=%q, want SQL NULL", tc.requestID, got)
		case !tc.wantNull && got != tc.want:
			t.Fatalf("%s: agent_client=%q, want %q", tc.requestID, got, tc.want)
		}
	}
}

func TestBackfillAgentClientRoundWalksTheCursorInBatches(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	// Five attributable rows; batches of two must walk them oldest-first
	// under the cursor without missing or rescanning any.
	ids := []string{"walk-1", "walk-2", "walk-3", "walk-4", "walk-5"}
	headers := "stub-signature"
	for _, id := range ids {
		seedBackfillRow(t, db, id, &headers, nil)
	}

	var totalUpdated int
	var cursor uint
	for round := 1; ; round++ {
		if round > 5 {
			t.Fatal("pass did not terminate: cursor is not advancing")
		}
		lastID, scanned, updated, err := BackfillAgentClientRound(db, cursor, 2, stubRecognizer)
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
	if err := db.Model(&model.RequestLog{}).Where("agent_client IS NULL").Count(&nullCount).Error; err != nil {
		t.Fatalf("count NULL agent_client: %v", err)
	}
	if nullCount != 0 {
		t.Fatalf("rows left NULL: %d, want 0", nullCount)
	}

	// A pass over an attributed table is one empty round: nothing scanned,
	// nothing updated, no error.
	_, scanned, updated, err := BackfillAgentClientRound(db, 0, 2, stubRecognizer)
	if err != nil {
		t.Fatalf("converged round: %v", err)
	}
	if scanned != 0 || updated != 0 {
		t.Fatalf("converged round: scanned=%d updated=%d, want 0/0", scanned, updated)
	}
}

func TestBackfillAgentClientRoundUpdateYieldsToARacingWriter(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	headers := "stub-signature"
	seedBackfillRow(t, db, "race-row", &headers, nil)

	// Simulate the race the NULL guard exists for: while the round holds
	// the selected row, another writer attributes it first. The recognizer
	// runs inside the round's update window, so giving it the side effect
	// lands the racing write exactly between SELECT and UPDATE.
	racing := func(snapshot []byte) string {
		if string(snapshot) != headers {
			return ""
		}
		if err := db.Model(&model.RequestLog{}).
			Where("request_id = ?", "race-row").
			UpdateColumn("agent_client", "racing-writer").Error; err != nil {
			t.Fatalf("racing writer update: %v", err)
		}
		return "stub-tool"
	}

	_, _, updated, err := BackfillAgentClientRound(db, 0, 10, racing)
	if err != nil {
		t.Fatalf("BackfillAgentClientRound: %v", err)
	}
	if updated != 0 {
		t.Fatalf("updated=%d, want 0 — the guard must let the racing writer win", updated)
	}
	got, _ := readAgentClient(t, db, "race-row")
	if got != "racing-writer" {
		t.Fatalf("agent_client=%q, want the racing writer's %q untouched", got, "racing-writer")
	}
}

func TestBackfillAgentClientRoundNeverTouchesTheSessionColumn(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	headers := "stub-signature"
	seedBackfillRow(t, db, "sess-row", &headers, nil)

	if _, _, _, err := BackfillAgentClientRound(db, 0, 10, stubRecognizer); err != nil {
		t.Fatalf("BackfillAgentClientRound: %v", err)
	}

	var row model.RequestLog
	if err := db.Where("request_id = ?", "sess-row").First(&row).Error; err != nil {
		t.Fatalf("read sess-row: %v", err)
	}
	if row.AgentClient == nil || *row.AgentClient != "stub-tool" {
		t.Fatalf("agent_client=%v, want stub-tool", row.AgentClient)
	}
	if row.AgentSessionID != nil {
		t.Fatalf("agent_session_id=%q, want SQL NULL — the engine structurally cannot write a session", *row.AgentSessionID)
	}
}
