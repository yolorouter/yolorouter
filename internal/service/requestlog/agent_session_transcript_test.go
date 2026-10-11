// Service-layer tests for the transcript read path
// (agent_session_transcript.go): the degradation contracts only this layer
// can inject — a failing bodies lookup or a missing turns table must
// degrade blocks to "not recorded" rather than failing the whole
// transcript — plus the endpoint's paging-semantics contracts (TC-07) and
// the parity contract that pins the turns-backed read to the whole-session
// pass's output (the storage rework's "逐事件等价" acceptance): a session
// derived at write time, whether by the live path or the backfill, reads
// back as the exact blocks and system timeline the pure pass computes from
// the same rows and bodies.
package requestlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/agentturns"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// backfillTurns stands in for the startup pass over seeded history: every
// request without its turn batch gets one, through the production engine.
func backfillTurns(t *testing.T, db *gorm.DB) {
	t.Helper()
	if _, err := agentturns.BackfillAll(context.Background(), db); err != nil {
		t.Fatalf("backfill turns: %v", err)
	}
}

// liveWriteTurns stands in for the gateway recorder's write: each request's
// turn batch derived and inserted at "record time", oldest first (the order
// the live path writes in).
func liveWriteTurns(t *testing.T, db *gorm.DB, rows []model.RequestLog) {
	t.Helper()
	bodies := readAllBodies(t, db)
	for i := range rows {
		body := bodies[rows[i].RequestID]
		var reqBody, respBody, streamPath string
		if body != nil {
			reqBody, respBody, streamPath = body.RequestBody, body.ResponseBody, body.StreamBodyPath
		}
		if err := agentturns.RecordTurnsForRequest(db, agentturns.RequestInput{
			SessionID:        derefString(rows[i].AgentSessionID),
			RequestID:        rows[i].RequestID,
			RequestPK:        rows[i].ID,
			CreatedAt:        rows[i].CreatedAt,
			RequestBody:      reqBody,
			HasInlineReply:   respBody != "",
			HasStreamCapture: streamPath != "",
		}); err != nil {
			t.Fatalf("record turns for %s: %v", rows[i].RequestID, err)
		}
	}
}

// readAllBodies is the fixtures' body-row snapshot helper.
func readAllBodies(t *testing.T, db *gorm.DB) map[string]*model.RequestLogBody {
	t.Helper()
	rows, err := repository.GetRequestLogBodiesByRequestIDs(db, allRequestIDs(t, db))
	if err != nil {
		t.Fatalf("read bodies: %v", err)
	}
	return rows
}

// TestGetAgentSessionTranscriptDegradesWhenBodiesTableFails pins the
// bodies-half degradation: when the batched request_log_bodies lookup
// errors, the transcript still answers with every block present — the
// added messages (persisted in the turn rows) and row metadata survive,
// and only the reply halves that live in the body rows degrade to
// resend/none. Never a whole-transcript error.
func TestGetAgentSessionTranscriptDegradesWhenBodiesTableFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
	base := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "sess-d"

	for i, id := range []string{"req-d1", "req-d2"} {
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
	}
	// req-d1's reply was never captured inline — its attribution is the
	// resend supplement req-d2's write appended, which lives in the TURN
	// rows and therefore survives the bodies table disappearing.
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:   "req-d1",
		RequestBody: `{"messages":[{"role":"user","content":"q"}]}`,
	}); err != nil {
		t.Fatalf("upsert body req-d1: %v", err)
	}
	// req-d2 resends d1's reply as history; its own body carries one new
	// user message (whose reply IS captured inline).
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:    "req-d2",
		RequestBody:  `{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"r"},{"role":"user","content":"next"}]}`,
		ResponseBody: `{"content":[{"type":"text","text":"r2"}]}`,
	}); err != nil {
		t.Fatalf("upsert body req-d2: %v", err)
	}
	backfillTurns(t, db)
	testutil.DropTable(t, db, "request_log_bodies")

	tr, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("GetAgentSessionTranscript after a bodies-table failure: %v, want the degraded transcript (the whole transcript must not fail)", err)
	}
	if len(tr.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (every block survives the body failure)", len(tr.Blocks))
	}
	b := tr.Blocks[0]
	if b.RequestID != "req-d1" || b.StatusClass != repository.StatusSuccess || b.ModelName != "gpt-4o-mini" {
		t.Fatalf("d1 metadata = (%s, %s, %q), want (req-d1, success, gpt-4o-mini) — row metadata survives", b.RequestID, b.StatusClass, b.ModelName)
	}
	if len(b.Added) != 1 || b.Added[0].(map[string]any)["content"] != "q" {
		t.Fatalf("d1 added = %+v, want the persisted input message (turn rows are unaffected by the bodies table)", b.Added)
	}
	// The reply attribution for the unrecorded turn is the resend copy the
	// successor's supplement row carries — persisted beside the added
	// messages, so a bodies-table failure does not touch it.
	if b.Assistant.Source != ReplaySourceResend {
		t.Fatalf("d1 assistant = %+v, want resend (the supplement answers from the turn rows)", b.Assistant)
	}
	copies, ok := b.Assistant.Raw.([]any)
	if !ok || len(copies) != 1 || copies[0].(map[string]any)["content"] != "r" {
		t.Fatalf("d1 resend raw = %+v, want the one resent copy", b.Assistant.Raw)
	}
	// The turn whose reply WAS captured inline degrades to "not recorded"
	// instead — the supplement arm never applies to an inline-captured turn,
	// so the body row was its only source.
	if d2 := tr.Blocks[1]; d2.Assistant.Source != ReplaySourceNone {
		t.Fatalf("d2 assistant = %+v, want none (its inline text is unreachable and no supplement applies)", d2.Assistant)
	}
	if tr.Session.RequestCount != 2 || tr.Session.SuccessCount != 2 {
		t.Fatalf("session counts = (%d, %d), want (2, 2) — the aggregate survives", tr.Session.RequestCount, tr.Session.SuccessCount)
	}
}

// TestGetAgentSessionTranscriptDegradesWithoutTurnsTable pins the
// turns-half degradation: a session whose turn rows are missing entirely
// (history the backfill has not reached) still answers — blocks carry row
// metadata, the reply halves attribute from the body rows where captures
// exist, and only the added messages read as empty. The whole transcript
// never fails on a storage half.
func TestGetAgentSessionTranscriptDegradesWithoutTurnsTable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
	base := time.Date(2026, 10, 9, 19, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "sess-nt"

	testutil.SeedRequestLog(t, db, "req-n1", base, func(r *model.RequestLog) {
		r.AgentClient = &claude
		r.AgentSessionID = &sess
		r.StatusCode = 200
	})
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:    "req-n1",
		RequestBody:  `{"messages":[{"role":"user","content":"q"}]}`,
		ResponseBody: `{"content":[{"type":"text","text":"r"}]}`,
	}); err != nil {
		t.Fatalf("upsert body: %v", err)
	}
	// No turn rows at all — the table itself is gone, which also forces the
	// system-meta and per-block turn lookups onto their error arms.
	testutil.DropTable(t, db, "agent_session_turns")

	tr, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("GetAgentSessionTranscript without turn rows: %v, want the degraded transcript", err)
	}
	if len(tr.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(tr.Blocks))
	}
	b := tr.Blocks[0]
	if len(b.Added) != 0 {
		t.Fatalf("added = %+v, want empty (nothing was derived)", b.Added)
	}
	if b.Assistant.Source != ReplaySourceInline || b.Assistant.Raw != `{"content":[{"type":"text","text":"r"}]}` {
		t.Fatalf("assistant = %+v, want (inline, the body row's reply)", b.Assistant)
	}
	if len(tr.System) != 0 {
		t.Fatalf("system meta = %+v, want empty", tr.System)
	}
}

// TestGetAgentSessionTranscriptPagingPreservesSemantics pins TC-07: the
// transcript endpoint slices only the OUTPUT — a cursor walk over a
// 25-request session reassembles to the whole-session blocks exactly, the
// page-tail resend fallback keeps working, and every page carries the FULL
// session aggregate and system timeline metadata.
//
// Fixture shape: request i's body carries [u1,a1,...,a_{i-1},u_i] (resent
// assistant copies ride the new suffix, so an unrecorded reply still
// attributes via resend); req-15's reply is unrecorded (req-16 resends it)
// and under limit 10 the before-walk's second page ends exactly on req-15,
// so its resend must survive a page tail; req-20 is a byte-identical retry
// of req-19 (its block's added is empty); the system prompt switches from
// S-one to S-two at req-13 (the timeline must arrive complete on every
// page).
func TestGetAgentSessionTranscriptPagingPreservesSemantics(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
	base := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "hs-page"
	const n = 25

	// messages(i) = [u1,a1,...,a_{i-1},u_i]: alternate user/assistant pairs
	// through i, then drop the trailing assistant so the body ends on u_i.
	messages := func(upto int) string {
		parts := make([]string, 0, 2*upto)
		for j := 1; j <= upto; j++ {
			parts = append(parts,
				fmt.Sprintf(`{"role":"user","content":"q%d"}`, j),
				fmt.Sprintf(`{"role":"assistant","content":"r%d"}`, j))
		}
		return strings.Join(parts[:len(parts)-1], ",")
	}
	var lastBody string
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("req-%02d", i)
		sys := "S-one"
		if i >= 13 {
			sys = "S-two"
		}
		reqBody := fmt.Sprintf(`{"system":%q,"messages":[%s]}`, sys, messages(i))
		if i == 20 {
			reqBody = lastBody // pure retry of req-19
		}
		resp := fmt.Sprintf(`{"content":[{"type":"text","text":"reply-%d"}]}`, i)
		if i == 15 {
			resp = "" // unrecorded reply — attributes via req-16's resent copy
		}
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:    id,
			RequestBody:  reqBody,
			ResponseBody: resp,
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
		lastBody = reqBody
	}
	backfillTurns(t, db)

	// The full reference: one call at the max limit holds all 25 blocks.
	full, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{Limit: 200})
	if err != nil {
		t.Fatalf("full transcript: %v", err)
	}
	if len(full.Blocks) != n {
		t.Fatalf("full transcript blocks = %d, want %d", len(full.Blocks), n)
	}
	if full.HasMoreBefore || full.HasMoreAfter {
		t.Fatalf("full transcript reports more blocks (%v/%v)", full.HasMoreBefore, full.HasMoreAfter)
	}

	// The cursor walk: latest page, then before-continuations to the head.
	// Pages arrive oldest→newest, so each older page prepends to the walk.
	page, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{Limit: 10})
	if err != nil {
		t.Fatalf("latest page: %v", err)
	}
	if len(page.Blocks) != 10 || page.Blocks[0].RequestID != "req-16" || !page.HasMoreBefore || page.NextBefore != "req-16" {
		t.Fatalf("latest page = %d blocks from %s (more=%v next=%q), want 10 from req-16 (true, req-16)",
			len(page.Blocks), page.Blocks[0].RequestID, page.HasMoreBefore, page.NextBefore)
	}
	walked := append([]AgentSessionReplayBlock{}, page.Blocks...)
	for page.HasMoreBefore {
		page, err = svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{Limit: 10, Before: page.NextBefore})
		if err != nil {
			t.Fatalf("before page: %v", err)
		}
		walked = append(append([]AgentSessionReplayBlock{}, page.Blocks...), walked...)
	}
	if len(walked) != n || !reflect.DeepEqual(walked, full.Blocks) {
		t.Fatalf("paged walk (len %d) ≠ whole-session blocks (len %d): paging must slice the finished sequence, never recompute it", len(walked), len(full.Blocks))
	}

	// The page-tail resend: the second page (before=req-16) ends on req-15,
	// whose reply was never recorded — its attribution must still be the
	// resent copy from req-16, exactly as the full output gives it.
	fullTail := replayBlockByID(t, full, "req-15")
	if fullTail.Assistant.Source != ReplaySourceResend {
		t.Fatalf("req-15 assistant source = %q, want resend (the write-time fallback)", fullTail.Assistant.Source)
	}
	page2, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{Limit: 10, Before: "req-16"})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.Blocks) != 10 || page2.Blocks[9].RequestID != "req-15" {
		t.Fatalf("page 2 = %d blocks ending at %s, want 10 ending at req-15", len(page2.Blocks), page2.Blocks[len(page2.Blocks)-1].RequestID)
	}
	if !reflect.DeepEqual(page2.Blocks[9].Assistant, fullTail.Assistant) {
		t.Fatalf("page-tail req-15 assistant = %+v, want the full-pass resend %+v", page2.Blocks[9].Assistant, fullTail.Assistant)
	}

	// The pure retry: req-20's added is empty and its own reply is inline.
	retry := replayBlockByID(t, full, "req-20")
	if len(retry.Added) != 0 || retry.AddedTruncated {
		t.Fatalf("req-20 added = %+v (truncated=%v), want empty (pure retry)", retry.Added, retry.AddedTruncated)
	}
	if retry.Assistant.Source != ReplaySourceInline {
		t.Fatalf("req-20 assistant source = %q, want inline", retry.Assistant.Source)
	}

	// Every page carries the WHOLE session aggregate and system metadata —
	// the header numbers never depend on how much is loaded.
	if page2.Session.RequestCount != n || page2.Session.SuccessCount != n {
		t.Fatalf("page 2 aggregate = (%d requests, %d success), want (%d, %d) — never sliced", page2.Session.RequestCount, page2.Session.SuccessCount, n, n)
	}
	if len(page2.System) != 2 || page2.System[0].SeenAtRequest != "req-01" || page2.System[1].SeenAtRequest != "req-13" {
		t.Fatalf("page 2 system timeline = %+v, want both entries (req-01, req-13) on every page", page2.System)
	}
	if page2.System[0].RawLength != len(`"S-one"`) || page2.System[1].RawLength != len(`"S-two"`) {
		t.Fatalf("page 2 system raw_length = (%d, %d), want the JSON-serialized sizes (%d, %d)",
			page2.System[0].RawLength, page2.System[1].RawLength, len(`"S-one"`), len(`"S-two"`))
	}
}

// transcriptFixture is the shared 25-request conversation the parity and
// degradation tests replay: alternating user/assistant turns with resent
// copies, a system switch, one unrecorded reply, one pure retry, plus the
// message kinds the write-time classification meets in the wild (an
// injected system-reminder, a tool result, a tool_use-bearing resent copy).
type transcriptFixture struct {
	db   *gorm.DB
	svc  *RequestLogService
	sess string
}

// newTranscriptFixture seeds rows+bodies (NO turn rows — the caller picks
// which write path derives them) and returns the handle.
func newTranscriptFixture(t *testing.T, name string) *transcriptFixture {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	base := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := name
	const n = 25

	// history(i) = the full resent history request i carries: alternating
	// pairs [u1,a1,...,u_i], with the variant messages spliced in.
	history := func(upto int) []string {
		parts := make([]string, 0, 2*upto+3)
		for j := 1; j <= upto; j++ {
			parts = append(parts,
				fmt.Sprintf(`{"role":"user","content":"q%d"}`, j),
				fmt.Sprintf(`{"role":"assistant","content":"r%d"}`, j))
		}
		return parts
	}
	var lastBody string
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("req-%02d", i)
		sys := `"S-one"`
		if i >= 13 {
			sys = `"S-two"`
		}
		msgs := history(i)
		// The variant messages ride specific requests' new suffixes.
		switch i {
		case 5:
			msgs = append(msgs[:len(msgs)-1],
				`{"role":"user","content":"<system-reminder>you have a reminder</system-reminder>"}`,
				`{"role":"user","content":"q5"}`)
		case 7:
			msgs = append(msgs[:len(msgs)-1],
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-1","content":"file contents"}]}`,
				`{"role":"user","content":"q7"}`)
		case 17:
			// The resent copy for req-16's unrecorded reply carries a
			// tool_use block — the tool_call event shape.
			msgs[len(msgs)-1] = `{"role":"assistant","content":[{"type":"tool_use","id":"tu-1","name":"Read","input":{"path":"/x"}}]}`
		}
		reqBody := fmt.Sprintf(`{"system":%s,"messages":[%s]}`, sys, strings.Join(msgs, ","))
		if i == 20 {
			reqBody = lastBody // pure retry of req-19
		}
		resp := fmt.Sprintf(`{"content":[{"type":"text","text":"reply-%d"}]}`, i)
		if i == 15 || i == 16 {
			resp = "" // unrecorded replies — attribute via the successors' resends
		}
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:    id,
			RequestBody:  reqBody,
			ResponseBody: resp,
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
		lastBody = reqBody
	}
	return &transcriptFixture{db: db, svc: NewRequestLogService(db, ""), sess: sess}
}

// rows returns the session's request rows in read order.
func (f *transcriptFixture) rows(t *testing.T) []model.RequestLog {
	t.Helper()
	rows, err := repository.ListRequestLogsByAgentSession(f.db, f.sess)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	return rows
}

// wholeSessionPassOracle computes the pre-rework reference: the pure pass
// over the same rows and bodies the old load-and-assemble read, with the
// same inline caps it applied. This is the specification output the turns
// read path must reproduce exactly.
func (f *transcriptFixture) wholeSessionPassOracle(t *testing.T) (blocks []AgentSessionReplayBlock, system []AgentSessionReplaySystem) {
	t.Helper()
	rows := f.rows(t)
	bodies := readAllBodies(t, f.db)
	inputs := make([]AgentSessionReplayInput, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		in := AgentSessionReplayInput{
			RequestID:    r.RequestID,
			CreatedAt:    r.CreatedAt,
			ModelName:    r.ModelName,
			StatusClass:  DeriveStatusClass(r.StatusCode, r.FailReason),
			IsStream:     r.IsStream,
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			DurationMs:   r.DurationMs,
		}
		if b := bodies[r.RequestID]; b != nil {
			in.RequestBody = truncateInlineBody(b.RequestBody)
			in.RequestBodyTruncated = len(b.RequestBody) > maxInlineBodyBytes
			in.ResponseBody = truncateInlineBody(b.ResponseBody)
			in.ResponseBodyTruncated = len(b.ResponseBody) > maxInlineBodyBytes
		}
		inputs = append(inputs, in)
	}
	return AssembleAgentSessionReplay(inputs)
}

// wipeTurns empties the session's turn rows so another write path can
// derive them from scratch.
func (f *transcriptFixture) wipeTurns(t *testing.T) {
	t.Helper()
	if err := f.db.Where("agent_session_id = ?", f.sess).Delete(&model.AgentSessionTurn{}).Error; err != nil {
		t.Fatalf("wipe turns: %v", err)
	}
}

// TestGetAgentSessionTranscriptMatchesWholeSessionPass is the storage
// rework's parity acceptance: for one mixed fixture, the turns-backed read
// equals the whole-session pass's blocks and system timeline EXACTLY — and
// it does so whichever write path derived the turns (the live write, one
// request at a time, or the startup backfill over the stored history).
func TestGetAgentSessionTranscriptMatchesWholeSessionPass(t *testing.T) {
	f := newTranscriptFixture(t, "hs-parity")

	readAll := func() (blocks []AgentSessionReplayBlock, system []AgentSessionReplaySystemMeta) {
		tr, err := f.svc.GetAgentSessionTranscript(f.sess, AgentSessionTranscriptPage{Limit: 200})
		if err != nil {
			t.Fatalf("transcript: %v", err)
		}
		return tr.Blocks, tr.System
	}

	oracleBlocks, oracleSystem := f.wholeSessionPassOracle(t)

	// Leg 1: the startup backfill derives the turns.
	backfillTurns(t, f.db)
	blocks, systemMeta := readAll()
	if !reflect.DeepEqual(blocks, oracleBlocks) {
		for i := range blocks {
			if i >= len(oracleBlocks) || !reflect.DeepEqual(blocks[i], oracleBlocks[i]) {
				t.Fatalf("backfill leg: block %d differs\n turns: %+v\n  pass: %+v", i, blocks[i], oracleBlocks[i])
			}
		}
		t.Fatalf("backfill leg: %d blocks ≠ %d oracle blocks", len(blocks), len(oracleBlocks))
	}
	assertSystemMetaMatches(t, systemMeta, oracleSystem)

	// Leg 2: the live write path derives the same turns from scratch, one
	// request at a time — the output must be identical.
	f.wipeTurns(t)
	liveWriteTurns(t, f.db, f.rows(t))
	blocks2, systemMeta2 := readAll()
	if !reflect.DeepEqual(blocks2, blocks) {
		t.Fatalf("live-write leg differs from backfill leg:\n live: %+v\n back: %+v", blocks2, blocks)
	}
	assertSystemMetaMatches(t, systemMeta2, oracleSystem)
}

// assertSystemMetaMatches checks the metadata list against the pass's raw
// timeline: index-aligned seen_at_request and raw_length == the raw value's
// serialized size.
func assertSystemMetaMatches(t *testing.T, meta []AgentSessionReplaySystemMeta, system []AgentSessionReplaySystem) {
	t.Helper()
	if len(meta) != len(system) {
		t.Fatalf("system timeline lengths = %d meta / %d pass, want equal", len(meta), len(system))
	}
	for i := range system {
		if meta[i].SeenAtRequest != system[i].SeenAtRequest {
			t.Fatalf("system[%d] seen_at_request = %q, want %q", i, meta[i].SeenAtRequest, system[i].SeenAtRequest)
		}
		serialized, err := json.Marshal(system[i].Raw)
		if err != nil || meta[i].RawLength != len(serialized) {
			t.Fatalf("system[%d] raw_length = %d, want %d", i, meta[i].RawLength, len(serialized))
		}
	}
}

// replayBlockByID finds one block by id in a transcript — the shared
// reference the paged assertions compare against.
func replayBlockByID(t *testing.T, tr *AgentSessionTranscript, requestID string) AgentSessionReplayBlock {
	t.Helper()
	for i := range tr.Blocks {
		if tr.Blocks[i].RequestID == requestID {
			return tr.Blocks[i]
		}
	}
	t.Fatalf("transcript carries no block %s", requestID)
	return AgentSessionReplayBlock{}
}

// TestGetAgentSessionTranscriptSystemRaw pins the system raw endpoint's half
// of the two-endpoint contract (TC-08's service leg): the raw timeline is
// the transcript metadata's payload, index-aligned with matching
// seen_at_request/truncated and raw_length == the raw's serialized size —
// and an unknown session answers the domain not-found error.
func TestGetAgentSessionTranscriptSystemRaw(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
	base := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "hs-sys"

	for i, sys := range []string{"first-prompt", "second-prompt", "second-prompt"} {
		id := fmt.Sprintf("req-s%d", i+1)
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:    id,
			RequestBody:  fmt.Sprintf(`{"system":%q,"messages":[{"role":"user","content":"q"}]}`, sys),
			ResponseBody: `{"content":[{"type":"text","text":"r"}]}`,
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
	}
	backfillTurns(t, db)

	tr, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	raw, err := svc.GetAgentSessionTranscriptSystem(sess)
	if err != nil {
		t.Fatalf("transcript system: %v", err)
	}
	if len(raw.System) != 2 || len(tr.System) != 2 {
		t.Fatalf("timeline lengths = raw %d / meta %d, want 2/2 (deduplicated)", len(raw.System), len(tr.System))
	}
	for i := range raw.System {
		if raw.System[i].SeenAtRequest != tr.System[i].SeenAtRequest {
			t.Fatalf("entry %d seen_at_request = raw %q / meta %q, want aligned", i, raw.System[i].SeenAtRequest, tr.System[i].SeenAtRequest)
		}
		if raw.System[i].Truncated != tr.System[i].Truncated {
			t.Fatalf("entry %d truncated = raw %v / meta %v, want aligned", i, raw.System[i].Truncated, tr.System[i].Truncated)
		}
		serialized, merr := json.Marshal(raw.System[i].Raw)
		if merr != nil || tr.System[i].RawLength != len(serialized) {
			t.Fatalf("entry %d raw_length = %d, want the raw's serialized size %d", i, tr.System[i].RawLength, len(serialized))
		}
	}
	if raw.System[0].Raw != "first-prompt" || raw.System[1].Raw != "second-prompt" {
		t.Fatalf("raw values = (%v, %v), want the two prompts in order", raw.System[0].Raw, raw.System[1].Raw)
	}

	if _, err := svc.GetAgentSessionTranscriptSystem("nope"); !errors.Is(err, errcode.ErrAgentSessionNotFound) {
		t.Fatalf("unknown session system endpoint err = %v, want ErrAgentSessionNotFound", err)
	}
}

// TestGetAgentSessionTranscriptSystemAtRequest pins the per-request form of
// the system raw fetch: the value IN EFFECT at the named request — its own
// row when the value changed there, the last earlier entry when it merely
// repeated it, the empty timeline for a request that precedes every
// recorded value or names no request of the session (the lenient
// stale-deep-link convention), and the domain not-found for an unknown
// session. The per-block System metadata must point at the same entry, so
// the block's collapse row and this fetch can never disagree.
func TestGetAgentSessionTranscriptSystemAtRequest(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
	base := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "hs-at"

	// Timeline shape: r0 has no system value; r1 records v1; r2 repeats v1
	// (no row of its own); r3 changes to v2.
	bodies := map[string]string{
		"r0": `{"system":null,"messages":[{"role":"user","content":"q0"}]}`,
		"r1": `{"system":"v1","messages":[{"role":"user","content":"q1"}]}`,
		"r2": `{"system":"v1","messages":[{"role":"user","content":"q2"}]}`,
		"r3": `{"system":"v2","messages":[{"role":"user","content":"q3"}]}`,
	}
	order := []string{"r0", "r1", "r2", "r3"}
	for i, id := range order {
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:   id,
			RequestBody: bodies[id],
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
	}
	backfillTurns(t, db)

	// Per-block metadata: the effective entry per request.
	tr, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if len(tr.Blocks) != 4 {
		t.Fatalf("blocks = %d, want 4", len(tr.Blocks))
	}
	if tr.Blocks[0].System != nil {
		t.Fatalf("r0 (no value in effect) System = %+v, want nil", tr.Blocks[0].System)
	}
	for i, wantSeen := range map[int]string{1: "r1", 2: "r1", 3: "r3"} {
		got := tr.Blocks[i].System
		if got == nil || got.SeenAtRequest != wantSeen {
			t.Fatalf("block %d System = %+v, want the entry recorded at %s", i, got, wantSeen)
		}
	}

	// The per-request fetch resolves to the same effective values.
	cases := []struct {
		requestID string
		wantRaw   any
		wantSeen  string
	}{
		{"r1", "v1", "r1"}, // own row
		{"r2", "v1", "r1"}, // inherited: repeats r1's value
		{"r3", "v2", "r3"}, // own change
		{"r0", nil, ""},    // precedes every recorded value
		{"zz", nil, ""},    // names no request of the session
	}
	for _, tc := range cases {
		got, err := svc.GetAgentSessionTranscriptSystemAtRequest(sess, tc.requestID)
		if err != nil {
			t.Fatalf("at-request %s: %v", tc.requestID, err)
		}
		if tc.wantRaw == nil {
			if len(got.System) != 0 {
				t.Fatalf("at-request %s: system = %+v, want the empty timeline", tc.requestID, got.System)
			}
			continue
		}
		if len(got.System) != 1 {
			t.Fatalf("at-request %s: %d entries, want exactly 1", tc.requestID, len(got.System))
		}
		if got.System[0].Raw != tc.wantRaw || got.System[0].SeenAtRequest != tc.wantSeen {
			t.Fatalf("at-request %s: entry = %+v, want raw %v recorded at %s", tc.requestID, got.System[0], tc.wantRaw, tc.wantSeen)
		}
	}

	// An empty request id falls back to the whole-timeline form.
	whole, err := svc.GetAgentSessionTranscriptSystemAtRequest(sess, "")
	if err != nil {
		t.Fatalf("whole-timeline form: %v", err)
	}
	if len(whole.System) != 2 {
		t.Fatalf("whole-timeline form: %d entries, want the 2 deduplicated values", len(whole.System))
	}

	if _, err := svc.GetAgentSessionTranscriptSystemAtRequest("nope", "r1"); !errors.Is(err, errcode.ErrAgentSessionNotFound) {
		t.Fatalf("unknown session err = %v, want ErrAgentSessionNotFound", err)
	}
}

// TestGetAgentSessionTranscriptRealByteBudgetWiring pins that the endpoint
// path uses the SHIPPED 1.5 MiB budget end to end (no test override): a
// block whose serialization alone exceeds it — near-cap added messages plus
// a near-cap inline reply, roughly a 2 MiB request body + response body —
// still ships whole as its own page, with the older direction truthful.
func TestGetAgentSessionTranscriptRealByteBudgetWiring(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
	base := time.Date(2026, 10, 10, 11, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "hs-big"

	seed := func(id string, minutes int, reqBody, respBody string) {
		t.Helper()
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(minutes)*time.Minute), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:    id,
			RequestBody:  reqBody,
			ResponseBody: respBody,
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
	}

	// The big block: a request body just UNDER the 1 MiB inline cap (so it
	// parses and its added message ships whole) plus a response body just
	// OVER the cap (so the inline copy caps at ~1 MiB) — together the block
	// serializes past the 1.5 MiB page budget.
	under := maxInlineBodyBytes - 128
	bigReq := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", under-45) + `"}]}`
	if len(bigReq) >= maxInlineBodyBytes {
		t.Fatalf("fixture: request body %d must stay under the inline cap %d", len(bigReq), maxInlineBodyBytes)
	}
	bigResp := `{"content":[{"type":"text","text":"` + strings.Repeat("b", maxInlineBodyBytes+4096) + `"}]}`
	seed("req-older", 0, `{"messages":[{"role":"user","content":"q"}]}`, `{"content":[{"type":"text","text":"small"}}]`)
	seed("req-big", 1, bigReq, bigResp)
	backfillTurns(t, db)

	tr, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if len(tr.Blocks) != 1 || tr.Blocks[0].RequestID != "req-big" {
		ids := make([]string, 0, len(tr.Blocks))
		for i := range tr.Blocks {
			ids = append(ids, tr.Blocks[i].RequestID)
		}
		t.Fatalf("latest page = %v, want [req-big] alone (its serialization tops the shipped budget)", ids)
	}
	if !tr.HasMoreBefore || tr.NextBefore != "req-big" {
		t.Fatalf("before continuation = (%v, %q), want (true, req-big)", tr.HasMoreBefore, tr.NextBefore)
	}
	if tr.HasMoreAfter {
		t.Fatalf("has_more_after = true, want false (nothing is newer)")
	}
}

// TestGetAgentSessionTranscriptTurnsPathPerformance pins the storage
// rework's performance acceptance on a realistic 72-request session (the
// replay batch's real-world shape: growing resent history, per-request
// input + reply, tool results, one system prompt): the COLD first
// transcript — no warmup of any kind — answers in <500ms and every
// before-continuation page in <100ms. The turns path reads only the page's
// turn rows, body rows, and capture bytes, so the numbers are one to two
// orders under the ceilings; the assertion exists to catch a regression
// that drags the whole session back into the read path.
func TestGetAgentSessionTranscriptTurnsPathPerformance(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
	base := time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)
	claude := "claude-code"
	sess := "hs-perf"
	const n = 72

	var history strings.Builder
	var lastBody string
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("req-%03d", i)
		var reqBody string
		if i == 1 {
			reqBody = `{"system":"You are a senior engineer. ` + strings.Repeat("grounding ", 1500) + `","messages":[{"role":"user","content":"kick off"}]}`
		} else {
			reqBody = lastBody[:len(lastBody)-2] + // reopen the messages array
				fmt.Sprintf(`,{"role":"user","content":"turn %d. %s"}`, i-1, strings.Repeat("please study the referenced files, cross-check the invariants, and continue with the next step of the plan. ", 48)) +
				`]}`
		}
		resp := `{"content":[{"type":"text","text":"` + strings.Repeat(fmt.Sprintf("reply-%d ", i), 8000) + `"}]}`
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Second), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:    id,
			RequestBody:  reqBody,
			ResponseBody: resp,
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
		lastBody = reqBody
	}
	_ = history
	backfillTurns(t, db)

	start := time.Now()
	tr, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	cold := time.Since(start)
	if err != nil {
		t.Fatalf("cold transcript: %v", err)
	}
	if len(tr.Blocks) != 20 || !tr.HasMoreBefore {
		t.Fatalf("cold page = %d blocks (more=%v), want 20 with more before", len(tr.Blocks), tr.HasMoreBefore)
	}
	t.Logf("cold first transcript (72-request session, no warmup): %v", cold)
	if cold >= 500*time.Millisecond {
		t.Fatalf("cold first transcript took %v, want <500ms (no warmup)", cold)
	}

	// Continuation pages: walk to the head; every page — full or the
	// short tail — must answer in <100ms.
	pages := 1
	walked := len(tr.Blocks)
	for tr.HasMoreBefore {
		start = time.Now()
		next, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{Limit: 20, Before: tr.NextBefore})
		pageDur := time.Since(start)
		if err != nil {
			t.Fatalf("before page %d: %v", pages, err)
		}
		if len(next.Blocks) == 0 {
			t.Fatalf("before page %d returned no blocks", pages)
		}
		t.Logf("before page %d (20 blocks): %v", pages, pageDur)
		if pageDur >= 100*time.Millisecond {
			t.Fatalf("before page %d took %v, want <100ms", pages, pageDur)
		}
		walked += len(next.Blocks)
		pages++
		tr = next
	}
	if walked != n {
		t.Fatalf("walk covered %d blocks over %d pages, want all %d", walked, pages, n)
	}
}

// allRequestIDs lists every request id in the seeded rows (fixtures are
// small enough that a full-table pluck is the honest shape).
func allRequestIDs(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var ids []string
	if err := db.Model(&model.RequestLog{}).Pluck("request_id", &ids).Error; err != nil {
		t.Fatalf("pluck request ids: %v", err)
	}
	return ids
}
