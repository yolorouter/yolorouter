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
	"reflect"
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
	e.derive = func(own OwnBody, prev *PrevBody, baseline SystemBaseline) []TurnEvent {
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
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "tf-1", RequestBody: b1})
	seedTurnFixtureRow(t, db, sess, "tf-2", base.Add(time.Minute))
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "tf-2", RequestBody: b2, ResponseBody: `{"content":[{"type":"text","text":"r2"}]}`})
	seedTurnFixtureRow(t, db, sess, "tf-3", base.Add(2*time.Minute))
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "tf-3", RequestBody: b2, ResponseBody: `{"content":[{"type":"text","text":"r3"}]}`})
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
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "c-1", RequestBody: b1})
	fixtureWrite(t, db, sess, "c-1", pk1, base, b1, "", "")

	// One pending-only request: a candidate.
	pk2 := seedTurnFixtureRow(t, db, sess, "c-2", base.Add(time.Minute))
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "c-2", RequestBody: b1})
	db.Create(&model.AgentSessionTurn{
		AgentSessionID: sess, RequestID: "c-2", RequestPK: pk2, RequestCreatedAt: base.Add(time.Minute),
		Seq: PendingSeq, EventType: EventPending,
	})

	// One request with ONLY a supplement (its successor was written live
	// before a deploy backfilled it): still a candidate.
	const b2 = `{"system":"S","messages":[{"role":"user","content":"q1"},{"role":"assistant","content":"r1"},{"role":"user","content":"q2"}]}`
	pk3 := seedTurnFixtureRow(t, db, sess, "c-3", base.Add(2*time.Minute))
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "c-3", RequestBody: b2})
	db.Create(&model.AgentSessionTurn{
		AgentSessionID: sess, RequestID: "c-3", RequestPK: pk3, RequestCreatedAt: base.Add(2 * time.Minute),
		Seq: SupplementSeqBase, EventType: EventAssistantReply,
		Content: `{"source":"resend","raw":[{"role":"assistant","content":"r1"}]}`,
	})

	// One untouched request: a candidate.
	pk4 := seedTurnFixtureRow(t, db, sess, "c-4", base.Add(3*time.Minute))
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "c-4", RequestBody: b2, ResponseBody: `{"content":[{"type":"text","text":"r"}]}`})
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
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "o-1", RequestBody: `{"messages":[{"role":"user","content":"1"}]}`})
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "o-2", RequestBody: `{"messages":[{"role":"user","content":"2"}]}`})
	repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "o-3", RequestBody: `{"messages":[{"role":"user","content":"3"}]}`})

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
