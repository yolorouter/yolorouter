// Service-layer tests for the transcript composition seam
// (agent_session_transcript.go): the degradation contracts only this layer
// can inject — a failing batched body lookup must degrade every block to
// "not recorded" rather than failing the whole transcript — plus the
// endpoint's paging-semantics contracts (TC-07): the pages a cursor
// walk produces concatenate back to the whole-session output with the
// resend fallback, retry-empty added, system metadata and session aggregate
// all intact on every page.
package requestlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// TestGetAgentSessionTranscriptDegradesWhenBodyBatchFails pins the
// body-batch failure branch: when the batched request_log_bodies lookup
// errors, the transcript still answers with every block carrying its row
// metadata, the bodies reading as "not recorded" — never a whole-transcript
// error. A recorded body row is seeded first (so the failing query is a
// failing lookup over REAL recordings, not an already-empty world), then the
// table is dropped to force the lookup error.
func TestGetAgentSessionTranscriptDegradesWhenBodyBatchFails(t *testing.T) {
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
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:    "req-d1",
		RequestBody:  `{"messages":[{"role":"user","content":"q"}]}`,
		ResponseBody: `{"content":[{"type":"text","text":"r"}]}`,
	}); err != nil {
		t.Fatalf("upsert body req-d1: %v", err)
	}
	testutil.DropTable(t, db, "request_log_bodies")

	tr, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("GetAgentSessionTranscript after a body-batch failure: %v, want the degraded transcript (the whole transcript must not fail)", err)
	}
	if len(tr.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (every block survives the body failure)", len(tr.Blocks))
	}
	b := tr.Blocks[0]
	if b.RequestID != "req-d1" || b.StatusClass != repository.StatusSuccess || b.ModelName != "gpt-4o-mini" {
		t.Fatalf("d1 metadata = (%s, %s, %q), want (req-d1, success, gpt-4o-mini) — row metadata survives", b.RequestID, b.StatusClass, b.ModelName)
	}
	if !b.CreatedAt.Equal(base) {
		t.Fatalf("d1 created_at = %v, want %v", b.CreatedAt, base)
	}
	if b.AddedTruncated {
		t.Fatalf("d1 added_truncated = true, want false (nothing was read, so nothing was cut)")
	}
	if len(b.Added) != 0 {
		t.Fatalf("d1 added = %+v, want empty (the body reads as not recorded)", b.Added)
	}
	if b.Assistant.Source != ReplaySourceNone || b.Assistant.Raw != nil {
		t.Fatalf("d1 assistant = %+v, want (none, nil raw)", b.Assistant)
	}
	if tr.Session.RequestCount != 2 || tr.Session.SuccessCount != 2 {
		t.Fatalf("session counts = (%d, %d), want (2, 2) — the aggregate survives", tr.Session.RequestCount, tr.Session.SuccessCount)
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
		t.Fatalf("paged walk (len %d) ≠ whole-session blocks (len %d): paging must slice the finished output, never recompute it", len(walked), len(full.Blocks))
	}

	// The page-tail resend: the second page (before=req-16) ends on req-15,
	// whose reply was never recorded — its attribution must still be the
	// resent copy from req-16, exactly as the full output gives it.
	fullTail := replayBlockByID(t, full, "req-15")
	if fullTail.Assistant.Source != ReplaySourceResend {
		t.Fatalf("req-15 assistant source = %q, want resend (the full-pass fallback)", fullTail.Assistant.Source)
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
	seed("req-older", 0, `{"messages":[{"role":"user","content":"q"}]}`, `{"content":[{"type":"text","text":"small"}}`)
	seed("req-big", 1, bigReq, bigResp)

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
