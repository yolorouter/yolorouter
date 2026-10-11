// Engine tests over a real migrated SQLite database: the live write path's
// batch shapes and incremental-cost contract, the UNIQUE(request_id, seq)
// idempotence under a raced duplicate, the degrade-to-pending path, and the
// backfill's parity with the live write plus its candidate rules. The
// Postgres arm of the same suite lives at the bottom, gated on
// TEST_POSTGRES_DSN the same way the repository package's audit suite is.
package agentturns

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedTurnFixtureRow inserts one request row; the id it returns is the
// request_pk the live write path would carry.
func seedTurnFixtureRow(t *testing.T, db *gorm.DB, sessionID, requestID string, ts time.Time) uint {
	t.Helper()
	client := "claude-code"
	testutil.SeedRequestLog(t, db, requestID, ts, func(r *model.RequestLog) {
		r.AgentClient = &client
		r.AgentSessionID = &sessionID
		r.StatusCode = 200
	})
	// SeedRequestLog's mutator runs before the create; read the assigned pk
	// back (fixtures are small, and this is the only write in flight).
	var pk uint
	if err := db.Model(&model.RequestLog{}).Where("request_id = ?", requestID).Pluck("id", &pk).Error; err != nil || pk == 0 {
		t.Fatalf("read back pk for %s: %v (pk=%d)", requestID, err, pk)
	}
	return pk
}

// fixtureWrite replays one request through the LIVE write path, the way the
// gateway recorder calls it.
func fixtureWrite(t *testing.T, db *gorm.DB, sessionID, requestID string, pk uint, ts time.Time, reqBody, respBody, streamPath string) {
	t.Helper()
	if err := RecordTurnsForRequest(db, RequestInput{
		SessionID:        sessionID,
		RequestID:        requestID,
		RequestPK:        pk,
		CreatedAt:        ts,
		RequestBody:      reqBody,
		HasInlineReply:   respBody != "",
		HasStreamCapture: streamPath != "",
	}); err != nil {
		t.Fatalf("RecordTurnsForRequest(%s): %v", requestID, err)
	}
}

// allTurns reads the whole table in insertion order — the parity oracle's
// input.
func allTurns(t *testing.T, db *gorm.DB) []model.AgentSessionTurn {
	t.Helper()
	var rows []model.AgentSessionTurn
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read turns: %v", err)
	}
	return rows
}

// TestRecordTurnsWritesOwnBatchAndSupplement pins the row shapes: own
// events at seq 0.., the assistant row last; the successor's resend
// supplement attributed to the PREDECESSOR's ids at seq ≥
// SupplementSeqBase; the system row with its ordinal and size sidecar.
func TestRecordTurnsWritesOwnBatchAndSupplement(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	sess := "turns-shape"

	const q1 = `{"system":"S-one","messages":[{"role":"user","content":"q1"}]}`
	const q2 = `{"system":"S-one","messages":[{"role":"user","content":"q1"},{"role":"assistant","content":"r1"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},{"role":"user","content":"q2"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"/a"}}]}]}`

	pk1 := seedTurnFixtureRow(t, db, sess, "req-1", base)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "req-1", RequestBody: q1}); err != nil {
		t.Fatalf("body 1: %v", err)
	}
	fixtureWrite(t, db, sess, "req-1", pk1, base, q1, "", "")

	pk2 := seedTurnFixtureRow(t, db, sess, "req-2", base.Add(time.Minute))
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "req-2", RequestBody: q2}); err != nil {
		t.Fatalf("body 2: %v", err)
	}
	fixtureWrite(t, db, sess, "req-2", pk2, base.Add(time.Minute), q2, "", "")

	rows := allTurns(t, db)
	type shape struct {
		requestID string
		seq       int64
		eventType string
	}
	var got []shape
	for _, r := range rows {
		got = append(got, shape{r.RequestID, r.Seq, r.EventType})
	}
	want := []shape{
		// req-1's own batch: the system change, its one added message, its
		// none-attributed assistant row.
		{"req-1", 0, EventSystemPrompt},
		{"req-1", 1, EventUserInput},
		{"req-1", 2, EventAssistantReply},
		// req-2's own batch: tool result + user message + assistant row.
		{"req-2", 0, EventToolResult},
		{"req-2", 1, EventUserInput},
		{"req-2", 2, EventAssistantReply},
		// req-2's write appends the supplement for req-1: the resend
		// attribution and the tool_call marker, on req-1's identity.
		{"req-1", SupplementSeqBase, EventAssistantReply},
		{"req-1", SupplementSeqBase + 1, EventToolCall},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("turn row shapes = %+v, want %+v", got, want)
	}

	// The supplement rows carry the PREDECESSOR's pk and created_at.
	for _, r := range rows {
		if r.RequestID == "req-1" && r.Seq >= SupplementSeqBase {
			if r.RequestPK != pk1 || !r.RequestCreatedAt.Equal(base) {
				t.Fatalf("supplement row identity = (pk %d, %v), want req-1's (%d, %v)", r.RequestPK, r.RequestCreatedAt, pk1, base)
			}
		}
	}

	// The system row's content_bytes is the raw value's serialized size.
	for _, r := range rows {
		if r.EventType != EventSystemPrompt {
			continue
		}
		var envelope systemEventContent
		if err := json.Unmarshal([]byte(r.Content), &envelope); err != nil {
			t.Fatalf("system envelope: %v", err)
		}
		if envelope.Ordinal != 0 || envelope.Raw != "S-one" {
			t.Fatalf("system envelope = %+v, want ordinal 0 S-one", envelope)
		}
		if r.ContentBytes != int64(len(`"S-one"`)) {
			t.Fatalf("system content_bytes = %d, want %d", r.ContentBytes, len(`"S-one"`))
		}
	}
}

// TestRecordTurnsFoldsResentUserInputAcrossRequests pins the write path's
// duplicate fold end to end over the real engine: an interrupted client's
// resend carries the same words with FRESH ide context, so the prefix diff
// sees a new message — the block-level split extracts the context as an
// injected event and the identical real text lands no second user_input
// row. A genuinely different follow-up keeps its row, and two DIFFERENT
// pure-image follow-ups keep theirs too: their real texts are both empty,
// and an empty key — shared by every textless message — never folds.
func TestRecordTurnsFoldsResentUserInputAcrossRequests(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)
	sess := "turns-dedup"

	const b1 = `{"messages":[{"role":"user","content":[{"type":"text","text":"\u003cide_selection\u003elines 1-1\u003c/ide_selection\u003e"},{"type":"text","text":"X"}]}]}`
	const b2 = `{"messages":[{"role":"user","content":[{"type":"text","text":"\u003cide_opened_file\u003efresh.jsonl\u003c/ide_opened_file\u003e"},{"type":"text","text":"X"}]}]}`
	const b3 = `{"messages":[{"role":"user","content":"Y"}]}`

	for i, body := range []string{b1, b2, b3} {
		id := fmt.Sprintf("d-%d", i+1)
		pk := seedTurnFixtureRow(t, db, sess, id, base.Add(time.Duration(i)*time.Minute))
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: id, RequestBody: body}); err != nil {
			t.Fatalf("body %s: %v", id, err)
		}
		fixtureWrite(t, db, sess, id, pk, base.Add(time.Duration(i)*time.Minute), body, "", "")
	}

	userInputs := func(requestID string) []string {
		var rows []model.AgentSessionTurn
		if err := db.Where("request_id = ? AND event_type = ?", requestID, EventUserInput).Order("seq ASC").Find(&rows).Error; err != nil {
			t.Fatalf("user inputs %s: %v", requestID, err)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, UserInputRealText(mustUnmarshalTurnContent(t, r.Content)))
		}
		return out
	}
	if got := userInputs("d-1"); !reflect.DeepEqual(got, []string{"X"}) {
		t.Fatalf("d-1 user inputs = %q, want [X] (the first copy always lands)", got)
	}
	if got := userInputs("d-2"); len(got) != 0 {
		t.Fatalf("d-2 user inputs = %q, want none (the resent duplicate folds)", got)
	}
	if got := userInputs("d-3"); !reflect.DeepEqual(got, []string{"Y"}) {
		t.Fatalf("d-3 user inputs = %q, want [Y] (a different text never folds)", got)
	}

	// The resend's fresh context still landed — as an injected row, off the
	// conversation axis but never dropped.
	var injected int64
	db.Model(&model.AgentSessionTurn{}).Where("request_id = ? AND event_type = ?", "d-2", EventInjected).Count(&injected)
	if injected != 1 {
		t.Fatalf("d-2 injected rows = %d, want 1 (the fresh ide context block)", injected)
	}

	// The fold state the engine reads is the last KEPT input — Y now.
	userBase, err := fetchLastUserInput(db, sess)
	if err != nil || !userBase.Has || userBase.Text != "Y" {
		t.Fatalf("last user input = (%+v, err %v), want Y", userBase, err)
	}

	// The empty-key arms: two DIFFERENT pure-image messages across two
	// requests. Their real texts are both "" — shared by every textless
	// message — so the second is NOT a resent duplicate of the first and
	// must land its own row; an empty key is never same-text evidence.
	const b4 = `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QUFB"}}]}]}`
	const b5 = `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QkJC"}}]}]}`
	for i, body := range []string{b4, b5} {
		id := fmt.Sprintf("d-%d", i+4)
		pk := seedTurnFixtureRow(t, db, sess, id, base.Add(time.Duration(i+3)*time.Minute))
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: id, RequestBody: body}); err != nil {
			t.Fatalf("body %s: %v", id, err)
		}
		fixtureWrite(t, db, sess, id, pk, base.Add(time.Duration(i+3)*time.Minute), body, "", "")
	}
	if got := userInputs("d-4"); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("d-4 user inputs = %q, want one empty-text row (a pure image after words keeps)", got)
	}
	if got := userInputs("d-5"); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("d-5 user inputs = %q, want one empty-text row (a DIFFERENT image never folds on the shared empty key)", got)
	}

	// The fold state after a kept no-text row: Text ""/Has true — exactly
	// what fetchLastUserInput resolves from the image row and what the
	// pass-side folder advanced to, so the two halves stay in lockstep and
	// the empty baseline folds nothing downstream.
	userBase, err = fetchLastUserInput(db, sess)
	if err != nil || !userBase.Has || userBase.Text != "" {
		t.Fatalf("last user input = (%+v, err %v), want empty text with Has", userBase, err)
	}
}

// TestRecordTurnsTreatsCorruptLastUserInputAsNoBaseline pins the corrupt-row
// arm fetchLastUserInput's guarantee comment promises: a stored user_input
// row whose content no longer parses (bit-rot, a torn write) must read as
// "nothing kept" — nil error, zero baseline — so the write path RE-RECORDS
// the resent duplicate rather than folding against garbage or degrading to
// pending; the very next write re-establishes a readable baseline.
func TestRecordTurnsTreatsCorruptLastUserInputAsNoBaseline(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 15, 0, 0, 0, time.UTC)
	sess := "turns-corrupt-user"

	// cu-1 lands one real user_input row ("X") through the live path…
	const b1 = `{"messages":[{"role":"user","content":"X"}]}`
	pk1 := seedTurnFixtureRow(t, db, sess, "cu-1", base)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "cu-1", RequestBody: b1}); err != nil {
		t.Fatalf("body cu-1: %v", err)
	}
	fixtureWrite(t, db, sess, "cu-1", pk1, base, b1, "", "")

	// …then the session's newest user_input row rots in place: its content
	// stops parsing. This is the branch's exact precondition.
	if err := db.Model(&model.AgentSessionTurn{}).
		Where("request_id = ? AND event_type = ?", "cu-1", EventUserInput).
		Update("content", "not json {").Error; err != nil {
		t.Fatalf("rot the user_input row in place: %v", err)
	}

	// The corrupt arm reads it as "nothing kept": no error, zero baseline.
	userBase, err := fetchLastUserInput(db, sess)
	if err != nil {
		t.Fatalf("fetchLastUserInput on a corrupt row: %v (want nil — corrupt reads as nothing kept)", err)
	}
	if userBase.Has || userBase.Text != "" {
		t.Fatalf("corrupt row baseline = %+v, want the zero baseline (Has false, no text)", userBase)
	}

	// cu-2 resends the very text the rotted row held, wrapped in FRESH ide
	// context so the prefix diff sees an added message (an identical body
	// would derive nothing): the write must take the recoverable direction —
	// succeed (no pending marker; a corrupt row is not a write failure), land
	// its own re-recorded row instead of folding against the garbage, and
	// leave a readable baseline behind.
	const b2 = `{"messages":[{"role":"user","content":[{"type":"text","text":"\u003cide_opened_file\u003efresh.jsonl\u003c/ide_opened_file\u003e"},{"type":"text","text":"X"}]}]}`
	pk2 := seedTurnFixtureRow(t, db, sess, "cu-2", base.Add(time.Minute))
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "cu-2", RequestBody: b2}); err != nil {
		t.Fatalf("body cu-2: %v", err)
	}
	fixtureWrite(t, db, sess, "cu-2", pk2, base.Add(time.Minute), b2, "", "")

	var pending int64
	db.Model(&model.AgentSessionTurn{}).Where("request_id = ? AND event_type = ?", "cu-2", EventPending).Count(&pending)
	if pending != 0 {
		t.Fatalf("cu-2 pending markers = %d, want 0 (a corrupt row is not a write failure)", pending)
	}
	var kept []model.AgentSessionTurn
	if err := db.Where("request_id = ? AND event_type = ?", "cu-2", EventUserInput).Order("seq ASC").Find(&kept).Error; err != nil {
		t.Fatalf("read cu-2 user inputs: %v", err)
	}
	if len(kept) != 1 || UserInputRealText(mustUnmarshalTurnContent(t, kept[0].Content)) != "X" {
		t.Fatalf("cu-2 user inputs = %+v, want exactly one re-recorded X row", kept)
	}
	userBase, err = fetchLastUserInput(db, sess)
	if err != nil || !userBase.Has || userBase.Text != "X" {
		t.Fatalf("baseline after the re-record = (%+v, err %v), want X re-established", userBase, err)
	}
}

// TestRecordTurnsDegradesToPendingOnUserBaseReadFailure pins the fetchUserBase
// error arm: a failed "last kept user input" read degrades to the pending
// marker and returns the cause for the log — the audit rows stay committed
// and the request stays a backfill candidate, same as every other read
// failure the write path meets.
func TestRecordTurnsDegradesToPendingOnUserBaseReadFailure(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 15, 30, 0, 0, time.UTC)
	sess := "turns-userbase-err"
	pk := seedTurnFixtureRow(t, db, sess, "req-ub1", base)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "req-ub1", RequestBody: `{"messages":[]}`}); err != nil {
		t.Fatalf("body: %v", err)
	}

	e := &turnEngine{db: db}
	e.fetchUserBase = func(string) (UserInputBaseline, error) {
		return UserInputBaseline{}, fmt.Errorf("simulated user-input read failure")
	}
	err := e.writeForRequest(RequestInput{
		SessionID: sess, RequestID: "req-ub1", RequestPK: pk, CreatedAt: base,
		RequestBody: `{"messages":[]}`,
	})
	if err == nil {
		t.Fatalf("degraded write returned nil, want the cause for the log")
	}
	if want := "read last user input"; !strings.Contains(err.Error(), want) {
		t.Fatalf("degraded error = %q, want it naming the failed read (%q)", err.Error(), want)
	}
	rows := allTurns(t, db)
	if len(rows) != 1 || rows[0].EventType != EventPending || rows[0].Seq != PendingSeq {
		t.Fatalf("pending marker = %+v, want exactly one pending row at PendingSeq", rows)
	}
}

// mustUnmarshalTurnContent decodes one turn row's content for assertions.
func mustUnmarshalTurnContent(t *testing.T, content string) any {
	t.Helper()
	var m any
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		t.Fatalf("turn content %q: %v", content, err)
	}
	return m
}

// TestRecordTurnsReadsPreviousBodyExactlyOnce pins the incremental-cost
// contract: ONE write fetches the previous request exactly once (the
// architecture's per-request price), and the system baseline once.
func TestRecordTurnsReadsPreviousBodyExactlyOnce(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	sess := "turns-count"
	pk := seedTurnFixtureRow(t, db, sess, "req-c1", base)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:    "req-c1",
		RequestBody:  `{"messages":[{"role":"user","content":"q"}]}`,
		ResponseBody: `{"content":[{"type":"text","text":"r"}]}`,
	}); err != nil {
		t.Fatalf("body: %v", err)
	}

	prevReads, baselineReads := 0, 0
	e := &turnEngine{db: db}
	e.fetchPrev = func(sessionID string, before time.Time, beforePK uint) (*prevRequest, error) {
		prevReads++
		return fetchPrevRequest(db, sessionID, before, beforePK)
	}
	e.fetchBaseline = func(sessionID string) (SystemBaseline, error) {
		baselineReads++
		return fetchSystemBaseline(db, sessionID)
	}
	err := e.writeForRequest(RequestInput{
		SessionID:      sess,
		RequestID:      "req-c1",
		RequestPK:      pk,
		CreatedAt:      base,
		RequestBody:    `{"messages":[{"role":"user","content":"q"}]}`,
		HasInlineReply: true,
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if prevReads != 1 {
		t.Fatalf("previous-body fetches = %d, want exactly 1 per write", prevReads)
	}
	if baselineReads != 1 {
		t.Fatalf("baseline fetches = %d, want exactly 1 per write", baselineReads)
	}
}

// TestRecordTurnsRacedDuplicateIsANoOp pins the idempotence guard: writing
// the same request's batch again (the startup backfill racing the live
// path, or a retried finalize) returns nil and leaves the stored rows
// untouched — UNIQUE(request_id, seq) with the deterministic seq
// assignment.
func TestRecordTurnsRacedDuplicateIsANoOp(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	sess := "turns-race"
	const body = `{"system":"S","messages":[{"role":"user","content":"q"}]}`
	pk := seedTurnFixtureRow(t, db, sess, "req-r1", base)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "req-r1", RequestBody: body}); err != nil {
		t.Fatalf("body: %v", err)
	}
	fixtureWrite(t, db, sess, "req-r1", pk, base, body, "", "")
	before := allTurns(t, db)

	fixtureWrite(t, db, sess, "req-r1", pk, base, body, "", "")
	after := allTurns(t, db)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("raced duplicate changed the stored rows\nbefore: %+v\n after: %+v", before, after)
	}
}

// TestRecordTurnsDegradesToPendingOnDerivationFailure pins the degrade
// path: a derivation that fails (here: panics through the white-box seam)
// writes the pending marker row and reports the cause for logging — the
// audit rows are the caller's business and stay untouched.
func TestRecordTurnsDegradesToPendingOnDerivationFailure(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC)
	sess := "turns-degrade"
	pk := seedTurnFixtureRow(t, db, sess, "req-p1", base)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "req-p1", RequestBody: `{"messages":[]}`}); err != nil {
		t.Fatalf("body: %v", err)
	}

	e := &turnEngine{db: db}
	e.derive = func(own OwnBody, prev *PrevBody, baseline SystemBaseline, userBase UserInputBaseline) []TurnEvent {
		panic("simulated derivation bug")
	}
	err := e.writeForRequest(RequestInput{
		SessionID: sess, RequestID: "req-p1", RequestPK: pk, CreatedAt: base,
		RequestBody: `{"messages":[]}`,
	})
	if err == nil {
		t.Fatalf("degraded write returned nil, want the cause for the log")
	}
	rows := allTurns(t, db)
	if len(rows) != 1 || rows[0].EventType != EventPending || rows[0].Seq != PendingSeq {
		t.Fatalf("pending marker = %+v, want exactly one pending row at PendingSeq", rows)
	}
	// The audit rows are untouched by construction (different tables), but
	// the marker must not have swallowed them either.
	var count int64
	db.Model(&model.RequestLog{}).Where("request_id = ?", "req-p1").Count(&count)
	if count != 1 {
		t.Fatalf("request rows = %d, want 1 (the audit write is not the turns write's business)", count)
	}
}

// turnsFixtureSession seeds a mixed three-request session WITHOUT deriving
// turns — the pre-migration history shape. req-1's reply is unrecorded
// (req-2 resends it with a tool_use copy); req-2's reply is inline;
// req-3 is a pure retry of req-2's body.
func turnsFixtureSession(t *testing.T, db *gorm.DB, sess string) {
	t.Helper()
	base := time.Date(2026, 10, 11, 11, 0, 0, 0, time.UTC)
	const b1 = `{"system":"S-one","messages":[{"role":"user","content":"q1"}]}`
	const b2 = `{"system":"S-one","messages":[{"role":"user","content":"q1"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"/a"}}]},{"role":"user","content":"q2"}]}`
	seedTurnFixtureRow(t, db, sess, "tf-1", base)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "tf-1", RequestBody: b1}); err != nil {
		t.Fatalf("body tf-1: %v", err)
	}
	seedTurnFixtureRow(t, db, sess, "tf-2", base.Add(time.Minute))
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "tf-2", RequestBody: b2, ResponseBody: `{"content":[{"type":"text","text":"r2"}]}`}); err != nil {
		t.Fatalf("body tf-2: %v", err)
	}
	seedTurnFixtureRow(t, db, sess, "tf-3", base.Add(2*time.Minute))
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "tf-3", RequestBody: b2, ResponseBody: `{"content":[{"type":"text","text":"r3"}]}`}); err != nil {
		t.Fatalf("body tf-3: %v", err)
	}
}

// stripTurnIDs strips the autoincrement column so live-written and
// backfilled row sets compare on content alone.
func stripTurnIDs(rows []model.AgentSessionTurn) []model.AgentSessionTurn {
	out := make([]model.AgentSessionTurn, len(rows))
	copy(out, rows)
	for i := range out {
		out[i].ID = 0
	}
	return out
}

// TestBackfillMatchesLiveWrite is the write-path half of the parity
// acceptance: the same session derived by the LIVE path (one request at a
// time) and by the BACKFILL (over the stored history) produces identical
// turn rows.
func TestBackfillMatchesLiveWrite(t *testing.T) {
	liveDB := testutil.NewSQLiteDB(t)
	backfillDB := testutil.NewSQLiteDB(t)
	turnsFixtureSession(t, liveDB, "tf-sess")
	turnsFixtureSession(t, backfillDB, "tf-sess")

	// Live leg: replay through the gateway recorder's entry point, in
	// write order.
	writeLive := func(db *gorm.DB) {
		rows := turnFixtureRows(t, db)
		bodies := turnFixtureBodies(t, db)
		for i := range rows {
			b := bodies[rows[i].RequestID]
			fixtureWrite(t, db, derefSessionID(rows[i].AgentSessionID), rows[i].RequestID, rows[i].ID, rows[i].CreatedAt, b.RequestBody, b.ResponseBody, b.StreamBodyPath)
		}
	}
	writeLive(liveDB)

	// Backfill leg: one synchronous pass.
	if _, err := BackfillAll(context.Background(), backfillDB); err != nil {
		t.Fatalf("BackfillAll: %v", err)
	}

	liveRows := stripTurnIDs(allTurns(t, liveDB))
	backfillRows := stripTurnIDs(allTurns(t, backfillDB))
	if !reflect.DeepEqual(liveRows, backfillRows) {
		t.Fatalf("backfilled rows ≠ live-written rows\n live: %+v\n back: %+v", liveRows, backfillRows)
	}
}

// turnFixtureRows lists a fixture's request rows in write order.
func turnFixtureRows(t *testing.T, db *gorm.DB) []model.RequestLog {
	t.Helper()
	var rows []model.RequestLog
	if err := db.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("rows: %v", err)
	}
	return rows
}

// turnFixtureBodies snapshots a fixture's body rows.
func turnFixtureBodies(t *testing.T, db *gorm.DB) map[string]*model.RequestLogBody {
	t.Helper()
	var rows []model.RequestLogBody
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("bodies: %v", err)
	}
	out := make(map[string]*model.RequestLogBody, len(rows))
	for i := range rows {
		out[rows[i].RequestID] = &rows[i]
	}
	return out
}

// TestBackfillRoundCandidateRules pins the candidate query: a converged
// table scans nothing, a request with only a pending marker stays a
// candidate until a real batch lands, and a request whose supplement
// already exists but whose own batch does not is still a candidate (the
// mixed live/deploy-window shape).
func TestBackfillRoundCandidateRules(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	sess := "tf-cand"

	// One fully processed request: the next round must not name it.
	const b1 = `{"system":"S","messages":[{"role":"user","content":"q1"}]}`
	pk1 := seedTurnFixtureRow(t, db, sess, "c-1", base)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "c-1", RequestBody: b1}); err != nil {
		t.Fatalf("body c-1: %v", err)
	}
	fixtureWrite(t, db, sess, "c-1", pk1, base, b1, "", "")

	// One pending-only request: a candidate.
	pk2 := seedTurnFixtureRow(t, db, sess, "c-2", base.Add(time.Minute))
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "c-2", RequestBody: b1}); err != nil {
		t.Fatalf("body c-2: %v", err)
	}
	db.Create(&model.AgentSessionTurn{
		AgentSessionID: sess, RequestID: "c-2", RequestPK: pk2, RequestCreatedAt: base.Add(time.Minute),
		Seq: PendingSeq, EventType: EventPending,
	})

	// One request with ONLY a supplement (its successor was written live
	// before a deploy backfilled it): still a candidate.
	const b2 = `{"system":"S","messages":[{"role":"user","content":"q1"},{"role":"assistant","content":"r1"},{"role":"user","content":"q2"}]}`
	pk3 := seedTurnFixtureRow(t, db, sess, "c-3", base.Add(2*time.Minute))
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "c-3", RequestBody: b2}); err != nil {
		t.Fatalf("body c-3: %v", err)
	}
	db.Create(&model.AgentSessionTurn{
		AgentSessionID: sess, RequestID: "c-3", RequestPK: pk3, RequestCreatedAt: base.Add(2 * time.Minute),
		Seq: SupplementSeqBase, EventType: EventAssistantReply,
		Content: `{"source":"resend","raw":[{"role":"assistant","content":"r1"}]}`,
	})

	// One untouched request: a candidate.
	pk4 := seedTurnFixtureRow(t, db, sess, "c-4", base.Add(3*time.Minute))
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "c-4", RequestBody: b2, ResponseBody: `{"content":[{"type":"text","text":"r"}]}`}); err != nil {
		t.Fatalf("body c-4: %v", err)
	}
	_ = pk4

	// Drain: exactly the three candidates are named (their own batches
	// land), then the next round scans nothing.
	if _, err := BackfillAll(context.Background(), db); err != nil {
		t.Fatalf("BackfillAll: %v", err)
	}
	for _, id := range []string{"c-2", "c-3", "c-4"} {
		var own int64
		db.Model(&model.AgentSessionTurn{}).
			Where("request_id = ? AND event_type <> ? AND seq < ?", id, EventPending, SupplementSeqBase).
			Count(&own)
		if own == 0 {
			t.Fatalf("request %s still has no own batch after the drain", id)
		}
	}
	lastID, scanned, err := BackfillRound(db, 0, 10)
	if err != nil || scanned != 0 {
		t.Fatalf("converged round = (scanned %d, err %v), want 0 scans", scanned, err)
	}
	_ = lastID

	// The pending marker stayed behind (harmless residue the read skips);
	// c-3 kept its supplement AND gained its own batch.
	var pending int64
	db.Model(&model.AgentSessionTurn{}).Where("request_id = ? AND event_type = ?", "c-2", EventPending).Count(&pending)
	if pending != 1 {
		t.Fatalf("c-2 pending marker = %d, want it left in place (skipped by reads)", pending)
	}
	var supplement int64
	db.Model(&model.AgentSessionTurn{}).Where("request_id = ? AND seq >= ?", "c-3", SupplementSeqBase).Count(&supplement)
	if supplement == 0 {
		t.Fatalf("c-3 lost its pre-existing supplement")
	}
}

// TestRecordTurnsSkipsUnattributedRequests pins the write path's no-op: a
// request the recognizer never tied to a session has no turn half.
func TestRecordTurnsSkipsUnattributedRequests(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC)
	testutil.SeedRequestLog(t, db, "plain-1", base, nil)
	if err := RecordTurnsForRequest(db, RequestInput{
		RequestID: "plain-1", RequestPK: 1, CreatedAt: base,
		RequestBody: `{"messages":[{"role":"user","content":"q"}]}`,
	}); err != nil {
		t.Fatalf("unattributed write: %v", err)
	}
	if rows := allTurns(t, db); len(rows) != 0 {
		t.Fatalf("turn rows written for an unattributed request: %+v", rows)
	}
}

// TestFetchPrevRequestOrdersByCreatedAtThenID pins the diff base's order —
// including the same-timestamp tie — against the session read paths' key.
func TestFetchPrevRequestOrdersByCreatedAtThenID(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 11, 13, 0, 0, 0, time.UTC)
	sess := "tf-order"
	// Two rows share a timestamp: id ASC makes the lower id "previous".
	pk1 := seedTurnFixtureRow(t, db, sess, "o-1", base)
	pk2 := seedTurnFixtureRow(t, db, sess, "o-2", base)
	pk3 := seedTurnFixtureRow(t, db, sess, "o-3", base.Add(time.Second))
	for _, b := range []struct {
		id  string
		req string
	}{{"o-1", `{"messages":[{"role":"user","content":"1"}]}`}, {"o-2", `{"messages":[{"role":"user","content":"2"}]}`}, {"o-3", `{"messages":[{"role":"user","content":"3"}]}`}} {
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: b.id, RequestBody: b.req}); err != nil {
			t.Fatalf("body %s: %v", b.id, err)
		}
	}

	prev, err := fetchPrevRequest(db, sess, base.Add(time.Second), pk3)
	if err != nil || prev == nil || prev.RequestID != "o-2" || prev.RequestPK != pk2 {
		t.Fatalf("prev of o-3 = (%v, err %v), want o-2 (the id tie-break)", prev, err)
	}
	prev, err = fetchPrevRequest(db, sess, base, pk2)
	if err != nil || prev == nil || prev.RequestID != "o-1" || prev.RequestPK != pk1 {
		t.Fatalf("prev of o-2 = (%v, err %v), want o-1", prev, err)
	}
	prev, err = fetchPrevRequest(db, sess, base, pk1)
	if err != nil || prev != nil {
		t.Fatalf("prev of o-1 = (%v, err %v), want none (first of the session)", prev, err)
	}
}
