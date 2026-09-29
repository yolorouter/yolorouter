package agentbackfill

import (
	"context"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// Historical masked header snapshots, spelled exactly as the sanitizer
// stored them BEFORE the tool-session allowlist existed: the dedicated tool
// headers are present with the redaction sentinel as their value, and the
// User-Agent — never a sensitive header — survived whole. These rows are
// the backfill's whole reason to exist: the tool is identifiable from
// header PRESENCE and the real UA, while the session value is gone.
const (
	histClaudeHeaders = `{"User-Agent":["claude-cli/2.1.0 (Ubuntu 24.04; x64)"],` +
		`"X-Claude-Code-Session-Id":["[REDACTED]"],"Cookie":["[REDACTED]"]}`
	histCodexHeaders = `{"user-agent":["codex/1.0.7"],"x-session-id":["[REDACTED]"]}`
	histCurlHeaders  = `{"User-Agent":["curl/7.81.0"]}`
)

// seedHistory inserts one request_logs row plus its body row carrying the
// stored header snapshot, optionally with columns already attributed.
func seedHistory(t *testing.T, db *gorm.DB, requestID, headers string, mut func(*model.RequestLog)) {
	t.Helper()
	testutil.SeedRequestLog(t, db, requestID, time.Now().UTC(), mut)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:      requestID,
		RequestHeaders: headers,
	}); err != nil {
		t.Fatalf("seed body row %s: %v", requestID, err)
	}
}

// readRow reads one full request_logs row back for assertions.
func readRow(t *testing.T, db *gorm.DB, requestID string) model.RequestLog {
	t.Helper()
	var row model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&row).Error; err != nil {
		t.Fatalf("read request_logs row %s: %v", requestID, err)
	}
	return row
}

func agentClientOf(t *testing.T, row model.RequestLog) (string, bool) {
	t.Helper()
	if row.AgentClient == nil {
		return "", true
	}
	return *row.AgentClient, false
}

// runPass drives one synchronous pass with the production recognizer —
// the same adapter NewTask installs when Config.Recognize is nil, asserted
// here so the default cannot silently drift from what these tests pin.
func runPass(t *testing.T, db *gorm.DB, batchSize int) {
	t.Helper()
	task := NewTask(Config{DB: db, BatchSize: batchSize})
	if task.recognize == nil {
		t.Fatal("NewTask left recognize nil — the production default is missing")
	}
	if reflect.ValueOf(task.recognize).Pointer() != reflect.ValueOf(recognizeAgentClient).Pointer() {
		t.Fatal("NewTask did not install the production recognizer by default")
	}
	task.Run(context.Background())
}

func TestBackfillPassAttributesHistoryAndNeverWritesASession(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	seedHistory(t, db, "hist-claude", histClaudeHeaders, nil)
	seedHistory(t, db, "hist-codex", histCodexHeaders, nil)
	seedHistory(t, db, "hist-curl", histCurlHeaders, nil)
	// A request with no body row at all: nothing was ever captured beside
	// it, so there is nothing to re-derive a name from — it stays NULL.
	testutil.SeedRequestLog(t, db, "hist-nobody", time.Now().UTC(), nil)
	// A row the live path already attributed: outside the NULL set, and it
	// must come through the pass byte-identical, session value included.
	seedHistory(t, db, "hist-done", histClaudeHeaders, func(row *model.RequestLog) {
		row.AgentClient = testutil.Ptr("gemini-cli")
		row.AgentSessionID = testutil.Ptr("live-session-keep")
	})

	// BatchSize 2 walks the five candidates in three rounds, exercising the
	// cursor path a one-round pass would never touch.
	runPass(t, db, 2)

	// The two recognizable tools get their names — claude via the real UA
	// (and the present-but-masked dedicated header), codex via its UA
	// prefix; a lowercased-snapshot spelling resolves the same.
	if got, isNull := agentClientOf(t, readRow(t, db, "hist-claude")); isNull || got != "claude-code" {
		t.Fatalf("hist-claude agent_client=%q null=%v, want claude-code", got, isNull)
	}
	if got, isNull := agentClientOf(t, readRow(t, db, "hist-codex")); isNull || got != "codex" {
		t.Fatalf("hist-codex agent_client=%q null=%v, want codex", got, isNull)
	}

	// The session column is the pollution guard the whole design hinges
	// on: history's masked sentinel must never land in it as a literal.
	// Both checks — SQL NULL, and not the sentinel — because either half
	// alone would let the other slip through.
	for _, requestID := range []string{"hist-claude", "hist-codex", "hist-curl", "hist-nobody"} {
		row := readRow(t, db, requestID)
		if row.AgentSessionID != nil {
			t.Fatalf("%s agent_session_id=%q, want SQL NULL", requestID, *row.AgentSessionID)
		}
	}

	// Unattributable and captureless history keeps its clean NULL: a value
	// in this column always means the tool identified itself.
	for _, requestID := range []string{"hist-curl", "hist-nobody"} {
		if got, isNull := agentClientOf(t, readRow(t, db, requestID)); !isNull {
			t.Fatalf("%s agent_client=%q, want SQL NULL — curl and no-capture rows must not be guessed", requestID, got)
		}
	}

	// Already-attributed rows are untouched, session included.
	done := readRow(t, db, "hist-done")
	if got, _ := agentClientOf(t, done); got != "gemini-cli" {
		t.Fatalf("hist-done agent_client=%q, want gemini-cli untouched", got)
	}
	if done.AgentSessionID == nil || *done.AgentSessionID != "live-session-keep" {
		t.Fatalf("hist-done agent_session_id=%v, want live-session-keep untouched", done.AgentSessionID)
	}
}

func TestBackfillPassIsIdempotentOnARerun(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	seedHistory(t, db, "rerun-claude", histClaudeHeaders, nil)
	seedHistory(t, db, "rerun-codex", histCodexHeaders, nil)

	runPass(t, db, 10)
	if got, isNull := agentClientOf(t, readRow(t, db, "rerun-claude")); isNull || got != "claude-code" {
		t.Fatalf("first pass: rerun-claude agent_client=%q null=%v, want claude-code", got, isNull)
	}

	// Zero-change rerun: every row, whole row, byte-identical. A second
	// pass over a drained table must not exist as far as the data is
	// concerned — not a new value, not a touched column, nothing.
	before := map[string]model.RequestLog{}
	for _, requestID := range []string{"rerun-claude", "rerun-codex"} {
		before[requestID] = readRow(t, db, requestID)
	}

	runPass(t, db, 10)

	for requestID, want := range before {
		if got := readRow(t, db, requestID); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed on rerun: before=%+v after=%+v", requestID, want, got)
		}
	}
}

func TestBackfillStartRunsThePassOnItsOwnGoroutineAndStopsCleanly(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	seedHistory(t, db, "async-claude", histClaudeHeaders, nil)

	task := NewTask(Config{DB: db, BatchSize: 10})
	stop := task.Start(context.Background())

	// The pass is finite; poll for its effect with a deadline so a broken
	// (non-terminating, never-writing) task fails the test instead of
	// hanging it. The stop function must then return promptly, and a
	// second call must be safe — the same contract the retention loop's
	// stop carries.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, isNull := agentClientOf(t, readRow(t, db, "async-claude")); !isNull && got == "claude-code" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background pass did not attribute async-claude within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	stop()
}

func TestBackfillPassWithACancelledContextChangesNothing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	seedHistory(t, db, "cancel-claude", histClaudeHeaders, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	NewTask(Config{DB: db, BatchSize: 10}).Run(ctx)

	row := readRow(t, db, "cancel-claude")
	if got, isNull := agentClientOf(t, row); !isNull {
		t.Fatalf("agent_client=%q after a cancelled pass, want SQL NULL — the residue must survive for the next startup", got)
	}
	if row.AgentSessionID != nil {
		t.Fatalf("agent_session_id=%q, want SQL NULL", *row.AgentSessionID)
	}
}
