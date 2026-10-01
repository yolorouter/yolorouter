package agentbackfill

import (
	"context"
	"errors"
	"reflect"
	"strings"
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
	seedHistoryBody(t, db, requestID, headers, "", mut)
}

// seedHistoryBody is seedHistory with the raw request body alongside the
// snapshot — the full stored capture the session pass reads.
func seedHistoryBody(t *testing.T, db *gorm.DB, requestID, headers, body string, mut func(*model.RequestLog)) {
	t.Helper()
	testutil.SeedRequestLog(t, db, requestID, time.Now().UTC(), mut)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:      requestID,
		RequestHeaders: headers,
		RequestBody:    body,
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

// runPass drives one synchronous pass with the production rules — the same
// adapters NewTask installs when Config.Recognize/ExtractSession are nil,
// asserted here so the defaults cannot silently drift from what these
// tests pin.
func runPass(t *testing.T, db *gorm.DB, batchSize int) {
	t.Helper()
	task := NewTask(Config{DB: db, BatchSize: batchSize})
	if task.recognize == nil {
		t.Fatal("NewTask left recognize nil — the production default is missing")
	}
	if reflect.ValueOf(task.recognize).Pointer() != reflect.ValueOf(recognizeAgentClient).Pointer() {
		t.Fatal("NewTask did not install the production recognizer by default")
	}
	if task.extractSession == nil {
		t.Fatal("NewTask left extractSession nil — the production default is missing")
	}
	if reflect.ValueOf(task.extractSession).Pointer() != reflect.ValueOf(ExtractAgentSession).Pointer() {
		t.Fatal("NewTask did not install the production session extractor by default")
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

// TestBackfillPassRestoresSessionsAfterTheClientPass is the second pass's
// marquee: one boot over the real historical shapes — a claude-code row
// whose session header was masked but whose verbatim body still carries
// metadata.user_id, and a codex row whose snapshot kept the unmasked
// turn-metadata blob while the body's client_metadata holds only the
// installation id — recovers BOTH columns, while a capture with no session
// carrier anywhere keeps a clean NULL and a live-path row stays untouched.
func TestBackfillPassRestoresSessionsAfterTheClientPass(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	claudeBodyText := string(claudeBody(quoted(claudeUserIDHistorical)))
	codexBodyText := string(codexBody(codexInstallationOnlyMetadata))

	seedHistoryBody(t, db, "v2-claude", histClaudeHeaders, claudeBodyText, nil)
	seedHistoryBody(t, db, "v2-codex", codexSnapshot, codexBodyText, nil)
	// Masked session header, no turn-metadata header, no body metadata:
	// nothing any rule can read — NULL survives, never the empty string.
	seedHistoryBody(t, db, "v2-codex-empty", histCodexHeaders, string(codexBody("")), nil)
	seedHistoryBody(t, db, "v2-done", histClaudeHeaders, claudeBodyText, func(row *model.RequestLog) {
		row.AgentClient = testutil.Ptr("claude-code")
		row.AgentSessionID = testutil.Ptr("live-session-keep")
	})

	runPass(t, db, 2)

	claude := readRow(t, db, "v2-claude")
	if got, isNull := agentClientOf(t, claude); isNull || got != "claude-code" {
		t.Fatalf("v2-claude agent_client=%q null=%v, want claude-code", got, isNull)
	}
	if claude.AgentSessionID == nil || *claude.AgentSessionID != "64148506-29da-4511-8892-034ac0cd18e9" {
		t.Fatalf("v2-claude agent_session_id=%v, want the uuid from the body's user_id", claude.AgentSessionID)
	}

	codex := readRow(t, db, "v2-codex")
	if got, isNull := agentClientOf(t, codex); isNull || got != "codex" {
		t.Fatalf("v2-codex agent_client=%q null=%v, want codex", got, isNull)
	}
	if codex.AgentSessionID == nil || *codex.AgentSessionID != codexThreadID {
		t.Fatalf("v2-codex agent_session_id=%v, want the snapshot blob's thread_id %q", codex.AgentSessionID, codexThreadID)
	}

	empty := readRow(t, db, "v2-codex-empty")
	if got, isNull := agentClientOf(t, empty); isNull || got != "codex" {
		t.Fatalf("v2-codex-empty agent_client=%q null=%v, want codex", got, isNull)
	}
	if empty.AgentSessionID != nil {
		t.Fatalf("v2-codex-empty agent_session_id=%q, want SQL NULL — no carrier means no ''", *empty.AgentSessionID)
	}

	done := readRow(t, db, "v2-done")
	if got, _ := agentClientOf(t, done); got != "claude-code" {
		t.Fatalf("v2-done agent_client=%q, want claude-code untouched", got)
	}
	if done.AgentSessionID == nil || *done.AgentSessionID != "live-session-keep" {
		t.Fatalf("v2-done agent_session_id=%v, want live-session-keep untouched", done.AgentSessionID)
	}
}

// TestBackfillPassDrivesTheClientPassBeforeTheSessionPass pins the order
// the two passes run in: the session extractor dispatches on the tool name
// the client pass writes, so a boot recovers both columns for a row that
// predates them only if every client round precedes every session round.
func TestBackfillPassDrivesTheClientPassBeforeTheSessionPass(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	seedHistoryBody(t, db, "order-row", "any-snapshot", "any-body", nil)

	var timeline []string
	recognize := func(snapshot []byte) string {
		timeline = append(timeline, "client")
		return "claude-code"
	}
	extract := func(agentClient string, body, headerSnapshot []byte) string {
		timeline = append(timeline, "session")
		return "stub-session"
	}
	NewTask(Config{DB: db, Recognize: recognize, ExtractSession: extract, BatchSize: 10}).
		Run(context.Background())

	seenSession := false
	for _, entry := range timeline {
		if entry == "session" {
			seenSession = true
		}
		if entry == "client" && seenSession {
			t.Fatalf("a client round ran after a session round: %v", timeline)
		}
	}
	if !seenSession {
		t.Fatalf("the session pass never ran: %v", timeline)
	}
	row := readRow(t, db, "order-row")
	if got, isNull := agentClientOf(t, row); isNull || got != "claude-code" {
		t.Fatalf("order-row agent_client=%q null=%v, want claude-code", got, isNull)
	}
	if row.AgentSessionID == nil || *row.AgentSessionID != "stub-session" {
		t.Fatalf("order-row agent_session_id=%v, want stub-session — the session pass must attribute on the client pass's output", row.AgentSessionID)
	}
}

// TestSessionPassFailureKeepsTheClientPassWritesAndDoesNotPanic injects the
// session pass's failure through the gorm callback seam (the
// model_service_test.go precedent): the callback fails exactly the updates
// that would set agent_session_id, so the client pass's updates (which set
// agent_client) complete first while every session write errors. Run
// returning at all is the no-panic half; the row assertions carry the
// "v1 writes survive" half.
//
// The hook goes BEFORE the update, on Dest's column map: an error added
// after a read-level callback would leave gorm's *sql.Rows unclosed, and
// the SQLite pool is capped at ONE connection (pkg/database), so a leaked
// rows set deadlocks every later statement instead of failing the pass.
func TestSessionPassFailureKeepsTheClientPassWritesAndDoesNotPanic(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	seedHistoryBody(t, db, "fail-row", histClaudeHeaders, string(claudeBody(quoted(claudeUserIDHistorical))), nil)

	if err := db.Callback().Update().Before("gorm:update").Register("test:fail-session-write", func(tx *gorm.DB) {
		dest, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok {
			return
		}
		if _, isSessionWrite := dest["agent_session_id"]; isSessionWrite {
			_ = tx.AddError(errors.New("simulated session backfill failure"))
		}
	}); err != nil {
		t.Fatalf("register update fault: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:fail-session-write"); err != nil {
			t.Fatalf("remove update fault: %v", err)
		}
	}()

	NewTask(Config{DB: db, BatchSize: 10}).Run(context.Background())

	row := readRow(t, db, "fail-row")
	if got, isNull := agentClientOf(t, row); isNull || got != "claude-code" {
		t.Fatalf("fail-row agent_client=%q null=%v, want claude-code — the client pass's writes must survive", got, isNull)
	}
	if row.AgentSessionID != nil {
		t.Fatalf("fail-row agent_session_id=%q, want SQL NULL — the failed pass wrote nothing", *row.AgentSessionID)
	}
}

// TestNewTaskAppliesBatchKnobsAndDrivesBothPassesWithThem pins the knob
// plumbing both ways. Structurally, NewTask must hold the configured
// values (both passes read these same fields at their engine calls), honor
// a zero BatchDelay (the no-pause test seam), and fall back to the
// defaults on zero/negative BatchSize and negative BatchDelay.
// Behaviorally, five rows with BatchSize 2 must walk THREE rounds per pass
// — counted on each pass's candidate SELECT through the gorm row callback
// (the engines Scan their batches, and Scan drives the row chain), since
// the client scan's SQL names agent_client and the session scan's names
// agent_session_id — and the shared throttle must show up as at least four
// inter-round pauses (two per pass; sleeps never wake early, so the lower
// bound cannot flake).
func TestNewTaskAppliesBatchKnobsAndDrivesBothPassesWithThem(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	// Zero BatchSize falls back; zero BatchDelay is honored as no pause.
	if task := NewTask(Config{DB: db}); task.batchSize != DefaultBatchSize || task.batchDelay != 0 {
		t.Fatalf("zero knobs: batchSize=%d batchDelay=%s, want %d/0s", task.batchSize, task.batchDelay, DefaultBatchSize)
	}
	// Negative knobs fall back to the defaults.
	if task := NewTask(Config{DB: db, BatchSize: -1, BatchDelay: -time.Nanosecond}); task.batchSize != DefaultBatchSize || task.batchDelay != DefaultBatchDelay {
		t.Fatalf("negative knobs: batchSize=%d batchDelay=%s, want %d/%s", task.batchSize, task.batchDelay, DefaultBatchSize, DefaultBatchDelay)
	}
	if task := NewTask(Config{DB: db, BatchSize: 3, BatchDelay: 7 * time.Millisecond}); task.batchSize != 3 || task.batchDelay != 7*time.Millisecond {
		t.Fatalf("configured knobs not held: batchSize=%d batchDelay=%s", task.batchSize, task.batchDelay)
	}

	for _, id := range []string{"knob-1", "knob-2", "knob-3", "knob-4", "knob-5"} {
		seedHistoryBody(t, db, id, "any-snapshot", "any-body", nil)
	}
	var clientRounds, sessionRounds int
	if err := db.Callback().Row().After("gorm:row").Register("test:count-backfill-rounds", func(tx *gorm.DB) {
		// Bare column names: the client scan's SQL never mentions
		// agent_session_id, the session scan's always does.
		switch {
		case strings.Contains(tx.Statement.SQL.String(), "agent_session_id"):
			sessionRounds++
		case strings.Contains(tx.Statement.SQL.String(), "agent_client"):
			clientRounds++
		}
	}); err != nil {
		t.Fatalf("register round counter: %v", err)
	}
	defer func() {
		if err := db.Callback().Row().Remove("test:count-backfill-rounds"); err != nil {
			t.Fatalf("remove round counter: %v", err)
		}
	}()

	recognize := func(snapshot []byte) string { return "claude-code" }
	extract := func(agentClient string, body, headerSnapshot []byte) string { return "stub-session" }
	start := time.Now()
	NewTask(Config{DB: db, Recognize: recognize, ExtractSession: extract, BatchSize: 2, BatchDelay: 25 * time.Millisecond}).
		Run(context.Background())
	elapsed := time.Since(start)

	if clientRounds != 3 {
		t.Fatalf("client pass rounds=%d, want 3 — BatchSize must reach the engine (2+2+1)", clientRounds)
	}
	if sessionRounds != 3 {
		t.Fatalf("session pass rounds=%d, want 3 — BatchSize must reach the engine (2+2+1)", sessionRounds)
	}
	if elapsed < 4*25*time.Millisecond {
		t.Fatalf("elapsed=%s, want at least 4x25ms of inter-round throttle (two pauses per pass)", elapsed)
	}
}
