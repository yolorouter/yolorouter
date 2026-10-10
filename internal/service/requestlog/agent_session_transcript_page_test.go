// Table tests for the transcript cursor-pagination slicer
// (agent_session_transcript_page.go). The slicer is pure over assembled
// blocks, so every contract lands here: latest page, exclusive before /
// inclusive after cursors, limit normalization, bad cursors, the byte-budget
// boundaries and the single-oversized-block exception, and both directions'
// exhaustion — the transcript endpoint's unit-layer acceptance cases.
package requestlog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pageTestBase is the shared timestamp origin for test blocks (one distinct
// instant per index keeps the sequence realistic and the order visible).
var pageTestBase = time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)

// pageBlock builds one replay block whose Added carries a padding string —
// each extra 'a' adds exactly one JSON byte, so a block's marshaled size is
// base + pad.
func pageBlock(id string, idx, pad int) AgentSessionReplayBlock {
	return AgentSessionReplayBlock{
		RequestID:   id,
		CreatedAt:   pageTestBase.Add(time.Duration(idx) * time.Minute),
		StatusClass: "success",
		Added: []any{map[string]any{
			"role":    "user",
			"content": strings.Repeat("a", pad),
		}},
	}
}

// blockOfMarshaledSize returns a block whose json.Marshal length is exactly
// size bytes — the padding is linear (one byte per 'a'), so one calibration
// measurement pins it. Sizes must exceed the bare block's own footprint.
func blockOfMarshaledSize(t *testing.T, id string, idx, size int) AgentSessionReplayBlock {
	t.Helper()
	b := pageBlock(id, idx, 0)
	base := marshaledBlockSize(b)
	if size < base {
		t.Fatalf("blockOfMarshaledSize %s: target %d is below the bare block size %d", id, size, base)
	}
	return pageBlock(id, idx, size-base)
}

// pageIDs collects a result's block ids in order — the assertable view of a
// page.
func pageIDs(res AgentSessionTranscriptPageResult) []string {
	ids := make([]string, 0, len(res.Blocks))
	for i := range res.Blocks {
		ids = append(ids, res.Blocks[i].RequestID)
	}
	return ids
}

// fiftyBlocks is a 50-block session with sortable zero-padded ids.
func fiftyBlocks() []AgentSessionReplayBlock {
	blocks := make([]AgentSessionReplayBlock, 50)
	for i := 0; i < 50; i++ {
		blocks[i] = pageBlock("b"+twoDigits(i), i, 3)
	}
	return blocks
}

func twoDigits(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// TestSliceTranscriptLatestPage pins TC-01: the no-cursor call returns the
// NEWEST limit blocks in chronological order with the older direction's
// continuation metadata and nothing after.
func TestSliceTranscriptLatestPage(t *testing.T) {
	res := SliceAgentSessionTranscriptBlocks(fiftyBlocks(), AgentSessionTranscriptPage{Limit: 20})

	if len(res.Blocks) != 20 {
		t.Fatalf("blocks = %d, want 20", len(res.Blocks))
	}
	// Newest 20 of 50 = blocks 31..50 (indices 30..49), oldest→newest order.
	if first, last := res.Blocks[0].RequestID, res.Blocks[19].RequestID; first != "b30" || last != "b49" {
		t.Fatalf("page spans [%s..%s], want [b30..b49] (the newest 20)", first, last)
	}
	for i := 1; i < len(res.Blocks); i++ {
		if res.Blocks[i].CreatedAt.Before(res.Blocks[i-1].CreatedAt) {
			t.Fatalf("page order broken at %d: blocks must stay chronological", i)
		}
	}
	if !res.HasMoreBefore || res.NextBefore != "b30" {
		t.Fatalf("before continuation = (%v, %q), want (true, b30 — the page's oldest block)", res.HasMoreBefore, res.NextBefore)
	}
	if res.HasMoreAfter || res.NextAfter != "" {
		t.Fatalf("after continuation = (%v, %q), want (false, \"\") — nothing is newer than the latest page", res.HasMoreAfter, res.NextAfter)
	}
}

// TestSliceTranscriptBeforeExclusiveToHead pins TC-02: before pages strictly
// OLDER than the cursor (the cursor block is never re-sent), and the walk
// ends with has_more_before=false and an empty next_before.
func TestSliceTranscriptBeforeExclusiveToHead(t *testing.T) {
	blocks := fiftyBlocks()

	// Continue from TC-01's next_before (b30).
	page2 := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 20, Before: "b30"})
	if got := pageIDs(page2); !reflect.DeepEqual(got, []string{"b10", "b11", "b12", "b13", "b14", "b15", "b16", "b17", "b18", "b19", "b20", "b21", "b22", "b23", "b24", "b25", "b26", "b27", "b28", "b29"}) {
		t.Fatalf("page 2 = %v, want b10..b29 (20 blocks strictly before b30)", got)
	}
	if page2.Blocks[len(page2.Blocks)-1].RequestID == "b30" {
		t.Fatalf("page 2 contains the cursor block — before is exclusive")
	}
	if !page2.HasMoreBefore || page2.NextBefore != "b10" {
		t.Fatalf("page 2 before continuation = (%v, %q), want (true, b10)", page2.HasMoreBefore, page2.NextBefore)
	}

	// The head page: 10 blocks remain, no continuation.
	page3 := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 20, Before: "b10"})
	if got := pageIDs(page3); len(got) != 10 || got[0] != "b00" || got[9] != "b09" {
		t.Fatalf("page 3 = %v, want b00..b09", got)
	}
	if page3.HasMoreBefore || page3.NextBefore != "" {
		t.Fatalf("head page before continuation = (%v, %q), want (false, \"\")", page3.HasMoreBefore, page3.NextBefore)
	}
	// Full before-walk coverage: page2+page3 concatenate (with page 1 from
	// TC-01) to the whole sequence without overlap or gap.
	if got := append(pageIDs(page3), pageIDs(page2)...); len(got) != 30 || got[0] != "b00" || got[29] != "b29" {
		t.Fatalf("paged walk covers b00..b29 without gaps, got %v", got)
	}
}

// TestSliceTranscriptAfterInclusive pins TC-03: an after page STARTS at the
// cursor block (inclusive — the anchor entry and the toward-newer
// continuation share the parameter) and reports the newest-block cursor.
func TestSliceTranscriptAfterInclusive(t *testing.T) {
	blocks := fiftyBlocks()

	res := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 20, After: "b09"})
	if len(res.Blocks) != 20 {
		t.Fatalf("blocks = %d, want 20", len(res.Blocks))
	}
	if res.Blocks[0].RequestID != "b09" {
		t.Fatalf("first block = %s, want b09 (after includes the cursor block)", res.Blocks[0].RequestID)
	}
	if res.Blocks[19].RequestID != "b28" {
		t.Fatalf("last block = %s, want b28", res.Blocks[19].RequestID)
	}
	if res.NextAfter != "b28" || !res.HasMoreAfter {
		t.Fatalf("after continuation = (%v, %q), want (true, b28 — the page's newest block)", res.HasMoreAfter, res.NextAfter)
	}
	if !res.HasMoreBefore || res.NextBefore != "b09" {
		t.Fatalf("before continuation = (%v, %q), want (true, b09)", res.HasMoreBefore, res.NextBefore)
	}
}

// TestSliceTranscriptCursorsExhaust pins TC-21: both directions page to their
// end and report has_more=false with an EMPTY next cursor, so a client can
// stop on either signal without a loop.
func TestSliceTranscriptCursorsExhaust(t *testing.T) {
	blocks := fiftyBlocks()

	// Toward newer from the head, pages of 3: the last page ends at b49.
	seen := 0
	loaded := make(map[string]bool)
	cursor := "b00"
	for {
		res := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 3, After: cursor})
		seen += len(res.Blocks)
		for i := range res.Blocks {
			loaded[res.Blocks[i].RequestID] = true
		}
		if !res.HasMoreAfter {
			if res.NextAfter != "" {
				t.Fatalf("exhausted after-walk: next_after = %q, want empty", res.NextAfter)
			}
			if res.Blocks[len(res.Blocks)-1].RequestID != "b49" {
				t.Fatalf("after-walk ended on %s, want b49", res.Blocks[len(res.Blocks)-1].RequestID)
			}
			break
		}
		cursor = res.NextAfter
	}
	// Inclusive after pages overlap by the anchor block each turn: pages of
	// 3 advance by 2 (limit minus the re-sent anchor), 49 blocks from b00 to
	// b49 → 25 pages, 24×3 + 2 = 74 copies. The overlap is the declared
	// inclusive-end cost; the client de-duplicates by request id.
	for i := 0; i < 50; i++ {
		if id := "b" + twoDigits(i); !loaded[id] {
			t.Fatalf("after-walk never delivered %s", id)
		}
	}
	if seen != 74 {
		t.Fatalf("after-walk delivered %d block copies, want 74 (50 + 24 inclusive anchors)", seen)
	}

	// Toward older from the tail (exclusive before, no overlap).
	seen = 0
	res := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 3})
	seen += len(res.Blocks)
	for res.HasMoreBefore {
		res = SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 3, Before: res.NextBefore})
		seen += len(res.Blocks)
	}
	if seen != 50 {
		t.Fatalf("before-walk delivered %d blocks, want 50 (exclusive cursors never overlap)", seen)
	}
	if res.NextBefore != "" {
		t.Fatalf("exhausted before-walk: next_before = %q, want empty", res.NextBefore)
	}
}

// TestSliceTranscriptLimitNormalization pins the unit half of TC-04: any
// limit that is not a positive integer up to 200 falls back to the default
// 20 (the page/page_size endpoints' silent-fallback precedent); 200 itself
// is a valid cap, not a clamp-to-20.
func TestSliceTranscriptLimitNormalization(t *testing.T) {
	blocks := fiftyBlocks()
	for _, tc := range []struct {
		limit int
		want  int
	}{
		{0, 20}, {-5, 20}, {201, 20}, {999999, 20}, // invalid/out-of-range → default
		{1, 1}, {20, 20}, {50, 50}, {200, 50}, // valid; 200 covers the whole fixture
	} {
		res := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: tc.limit})
		if len(res.Blocks) != tc.want {
			t.Fatalf("limit=%d: blocks = %d, want %d", tc.limit, len(res.Blocks), tc.want)
		}
	}
	// The shipped constants are the pinned numbers — a drift fails here.
	if transcriptDefaultLimit != 20 || transcriptMaxLimit != 200 || transcriptByteBudget != 1572864 {
		t.Fatalf("constants drifted: default=%d max=%d budget=%d, want 20/200/1572864", transcriptDefaultLimit, transcriptMaxLimit, transcriptByteBudget)
	}
}

// TestSliceTranscriptBadCursorEmptyPage pins TC-04's cursor arm at unit
// level: a cursor naming no block answers an empty page with BOTH has_more
// flags false and no continuation cursors (a stale deep link must not
// explode, and must read identically to "nothing left").
func TestSliceTranscriptBadCursorEmptyPage(t *testing.T) {
	for _, page := range []AgentSessionTranscriptPage{
		{Before: "no-such-block"},
		{After: "no-such-block"},
		{Limit: 5, Before: "stale-req-id"},
	} {
		res := SliceAgentSessionTranscriptBlocks(fiftyBlocks(), page)
		if res.Blocks == nil || len(res.Blocks) != 0 {
			t.Fatalf("%+v: blocks = %#v, want a non-nil empty slice", page, res.Blocks)
		}
		if res.HasMoreBefore || res.HasMoreAfter || res.NextBefore != "" || res.NextAfter != "" {
			t.Fatalf("%+v: continuation = (%v,%q,%v,%q), want all clear", page, res.HasMoreBefore, res.NextBefore, res.HasMoreAfter, res.NextAfter)
		}
	}
	// A KNOWN cursor with an empty window reads the same: before=<oldest
	// block> has nothing older to give.
	res := SliceAgentSessionTranscriptBlocks(fiftyBlocks(), AgentSessionTranscriptPage{Before: "b00"})
	if len(res.Blocks) != 0 || res.HasMoreBefore || res.HasMoreAfter {
		t.Fatalf("before=b00: got (%d blocks, more=%v/%v), want empty with both flags false", len(res.Blocks), res.HasMoreBefore, res.HasMoreAfter)
	}
}

// TestSliceTranscriptByteBudgetBoundaries pins TC-05 with a small injected
// budget: the walk stops BEFORE appending the block that would push the
// cumulative serialization to >= the budget — both at "exactly equals" and
// at "over by one" — and never rolls back an already-included block.
func TestSliceTranscriptByteBudgetBoundaries(t *testing.T) {
	const budget = 1000
	// A bare block marshals to a few hundred bytes; all fixtures here stay
	// comfortably above that floor.
	if base := marshaledBlockSize(pageBlock("probe", 0, 0)); base >= 600 {
		t.Fatalf("fixture floor moved: bare block = %d bytes; shrink the budget arms", base)
	}

	for _, tc := range []struct {
		name  string
		sizes []int
		want  []string
	}{
		// 600+400 = 1000 = budget exactly → the second block is refused.
		{"exactly equals budget stops the next block", []int{600, 400, 400}, []string{"b1"}},
		// 600+401 = 1001 = budget+1 → refused as well.
		{"over by one stops the next block", []int{600, 401, 400}, []string{"b1"}},
		// 600+399 = 999 < budget → both included; the third is refused
		// (already-included blocks are not rolled back).
		{"under budget includes both, then stops", []int{600, 399, 400}, []string{"b1", "b2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := make([]AgentSessionReplayBlock, len(tc.sizes))
			for i, size := range tc.sizes {
				blocks[i] = blockOfMarshaledSize(t, "b"+string(rune('1'+i)), i, size)
				if got := marshaledBlockSize(blocks[i]); got != size {
					t.Fatalf("fixture b%d marshals to %d bytes, want %d", i+1, got, size)
				}
			}
			res := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, After: "b1", ByteBudget: budget})
			if got := pageIDs(res); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("page = %v, want %v", got, tc.want)
			}
			if res.HasMoreAfter != (len(tc.want) < len(tc.sizes)) {
				t.Fatalf("has_more_after = %v, want %v", res.HasMoreAfter, len(tc.want) < len(tc.sizes))
			}
		})
	}
}

// TestSliceTranscriptOversizedSingleBlock pins TC-06: a single block whose
// serialization alone reaches the budget is never refused nor cut — it
// occupies its page alone (which may exceed the budget), and the older
// direction's has_more keeps telling the truth. A page that carries nothing
// beyond its anchor still advances the toward-newer walk: next_after steps
// past the anchor, and the continuation delivers the rest.
func TestSliceTranscriptOversizedSingleBlock(t *testing.T) {
	const budget = 1000
	huge := blockOfMarshaledSize(t, "huge", 0, 1200)
	if marshaledBlockSize(huge) != 1200 {
		t.Fatalf("fixture marshals to %d, want 1200", marshaledBlockSize(huge))
	}

	// From that block onward (anchor entry): it ships alone (the budget
	// break hits right after the first block). The page is nothing but its
	// anchor, so next_after must name the first block BEYOND the page —
	// re-anchoring inclusively on the anchor itself would deterministically
	// re-serve this same one-block page forever. Continuing the walk
	// delivers the rest and exhausts: the oversized ANCHOR advances.
	blocks := []AgentSessionReplayBlock{huge, pageBlock("after-1", 1, 3), pageBlock("after-2", 2, 3)}
	res := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, After: "huge", ByteBudget: budget})
	if got := pageIDs(res); !reflect.DeepEqual(got, []string{"huge"}) {
		t.Fatalf("anchor page = %v, want [huge] alone", got)
	}
	if !res.HasMoreAfter || res.NextAfter != "after-1" {
		t.Fatalf("after continuation = (%v, %q), want (true, after-1 — the block beyond the anchor-only page)", res.HasMoreAfter, res.NextAfter)
	}
	res = SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, After: res.NextAfter, ByteBudget: budget})
	if got := pageIDs(res); !reflect.DeepEqual(got, []string{"after-1", "after-2"}) {
		t.Fatalf("continuation page = %v, want [after-1 after-2] (the walk advanced past the oversized anchor)", got)
	}
	if res.HasMoreAfter || res.NextAfter != "" {
		t.Fatalf("continuation after flags = (%v, %q), want exhausted", res.HasMoreAfter, res.NextAfter)
	}

	// A NORMAL anchor whose successor is oversized: after=small walks up,
	// takes small, then huge is refused by the budget rule (an oversized
	// NON-first block is simply the walk's stop — not cut, not dropped from
	// the sequence). The page is again anchor-only, so next_after must step
	// PAST the anchor to huge; continuing after=huge then ships huge alone
	// and exhausts. Any anchor — normal or oversized — advances.
	small := pageBlock("small", 0, 3)
	blocks = []AgentSessionReplayBlock{small, huge}
	res = SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, After: "small", ByteBudget: budget})
	if got := pageIDs(res); !reflect.DeepEqual(got, []string{"small"}) {
		t.Fatalf("page from small = %v, want [small] (huge is refused by the budget, not cut or dropped from the sequence)", got)
	}
	if !res.HasMoreAfter || res.NextAfter != "huge" {
		t.Fatalf("after continuation = (%v, %q), want (true, huge — the block beyond the anchor-only page)", res.HasMoreAfter, res.NextAfter)
	}
	res = SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, After: res.NextAfter, ByteBudget: budget})
	if got := pageIDs(res); !reflect.DeepEqual(got, []string{"huge"}) {
		t.Fatalf("continuation page = %v, want [huge] alone (the refused oversized block ships as the next page's anchor)", got)
	}
	if res.HasMoreAfter || res.NextAfter != "" {
		t.Fatalf("continuation after flags = (%v, %q), want exhausted", res.HasMoreAfter, res.NextAfter)
	}

	// As the newest block (first screen): it ships alone, has_more_before
	// stays truthful about the older blocks.
	blocks = []AgentSessionReplayBlock{pageBlock("old-1", 0, 3), pageBlock("old-2", 1, 3), huge}
	res = SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, ByteBudget: budget})
	if got := pageIDs(res); !reflect.DeepEqual(got, []string{"huge"}) {
		t.Fatalf("latest page = %v, want [huge] alone", got)
	}
	if !res.HasMoreBefore || res.NextBefore != "huge" {
		t.Fatalf("before continuation = (%v, %q), want (true, huge)", res.HasMoreBefore, res.NextBefore)
	}
	if res.HasMoreAfter {
		t.Fatalf("has_more_after = true, want false (nothing is newer)")
	}
}

// TestSliceTranscriptBudgetTrimsTheFarSide pins where a budget stop cuts:
// an up-walk (after) drops the NEWER candidates beyond the stop; a down-walk
// (before/no-cursor) drops the OLDER candidates — the page always keeps the
// blocks nearest its anchor, so upscrolling never loses the turn the user is
// scrolling toward.
func TestSliceTranscriptBudgetTrimsTheFarSide(t *testing.T) {
	const budget = 1000
	// older → newer: b1(600) b2(600) b3(600) b4(small); cursor b4.
	blocks := []AgentSessionReplayBlock{
		blockOfMarshaledSize(t, "b1", 0, 600),
		blockOfMarshaledSize(t, "b2", 1, 600),
		blockOfMarshaledSize(t, "b3", 2, 600),
		blockOfMarshaledSize(t, "b4", 3, 400),
	}

	// before=b4: walk down from b3 — b3 taken (600), b2 would make 1200 →
	// the page is [b3] (nearest the cursor), not [b1].
	res := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, Before: "b4", ByteBudget: budget})
	if got := pageIDs(res); !reflect.DeepEqual(got, []string{"b3"}) {
		t.Fatalf("before page = %v, want [b3] (blocks nearest the cursor)", got)
	}
	if !res.HasMoreBefore || res.NextBefore != "b3" {
		t.Fatalf("before continuation = (%v, %q), want (true, b3)", res.HasMoreBefore, res.NextBefore)
	}

	// after=b1: walk up — b1 taken (600), b2 would make 1200 → [b1], an
	// anchor-only page, so next_after names the block BEYOND the page (b2)
	// rather than the anchor — re-anchoring on b1 would re-serve [b1]
	// forever.
	res = SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, After: "b1", ByteBudget: budget})
	if got := pageIDs(res); !reflect.DeepEqual(got, []string{"b1"}) {
		t.Fatalf("after page = %v, want [b1] (blocks nearest the anchor)", got)
	}
	if !res.HasMoreAfter || res.NextAfter != "b2" {
		t.Fatalf("after continuation = (%v, %q), want (true, b2 — the block beyond the anchor-only page)", res.HasMoreAfter, res.NextAfter)
	}

	// The budget walk then pages through the WHOLE window without gaps:
	// before=b3 → [b2], before=b2 → [b1], done.
	res = SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, Before: "b3", ByteBudget: budget})
	if got := pageIDs(res); !reflect.DeepEqual(got, []string{"b2"}) {
		t.Fatalf("second before page = %v, want [b2]", got)
	}
	res = SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, Before: "b2", ByteBudget: budget})
	if got := pageIDs(res); !reflect.DeepEqual(got, []string{"b1"}) || res.HasMoreBefore {
		t.Fatalf("final before page = %v (more=%v), want [b1] with no more", got, res.HasMoreBefore)
	}
}

// TestSliceTranscriptAfterWalkAdvancesOnBigBlockSessions pins the guarantee
// the anchor-only cursor rule exists for: in a session whose blocks are each
// legal alone but pairwise over the byte budget (two ~0.9 MiB capped bodies
// against the 1.5 MiB budget — exactly the huge-turn long session lazy
// paging exists to serve), every after page is its anchor alone, and the
// walk still delivers every block and reaches the tail: the cursor never
// repeats, so no continuation can re-serve the same page forever.
func TestSliceTranscriptAfterWalkAdvancesOnBigBlockSessions(t *testing.T) {
	const budget = 1000
	// Six blocks of 600 bytes: each fits alone, any two exceed the budget.
	blocks := make([]AgentSessionReplayBlock, 6)
	for i := range blocks {
		blocks[i] = blockOfMarshaledSize(t, "b"+twoDigits(i), i, 600)
	}

	seen := make(map[string]bool)
	cursor := "b00"
	for step := 0; ; step++ {
		if step > len(blocks) {
			t.Fatalf("after-walk needed more than %d steps — the cursor is not advancing", len(blocks))
		}
		res := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{Limit: 10, After: cursor, ByteBudget: budget})
		if got := pageIDs(res); !reflect.DeepEqual(got, []string{cursor}) {
			t.Fatalf("step %d: page = %v, want the anchor-only page [%s] (600+600 exceeds the budget, so the successor is refused)", step, got, cursor)
		}
		seen[cursor] = true
		if !res.HasMoreAfter {
			if res.NextAfter != "" {
				t.Fatalf("exhausted walk: next_after = %q, want empty", res.NextAfter)
			}
			if cursor != "b05" {
				t.Fatalf("walk ended on %s, want the tail block b05", cursor)
			}
			break
		}
		if res.NextAfter == cursor {
			t.Fatalf("step %d: next_after = %q repeats the cursor — the walk would stall on this page forever", step, res.NextAfter)
		}
		cursor = res.NextAfter
	}
	for i := 0; i < 6; i++ {
		if id := "b" + twoDigits(i); !seen[id] {
			t.Fatalf("after-walk never delivered %s", id)
		}
	}
}

// TestSliceTranscriptShortSessionWhole pins the small-session contract: a
// session that fits one page returns everything with both directions
// exhausted — no pagination state for the frontend to render.
func TestSliceTranscriptShortSessionWhole(t *testing.T) {
	blocks := make([]AgentSessionReplayBlock, 5)
	for i := range blocks {
		blocks[i] = pageBlock("s"+twoDigits(i), i, 3)
	}
	res := SliceAgentSessionTranscriptBlocks(blocks, AgentSessionTranscriptPage{})
	if len(res.Blocks) != 5 || res.Blocks[0].RequestID != "s00" || res.Blocks[4].RequestID != "s04" {
		t.Fatalf("short session page = %v, want all 5 in order", pageIDs(res))
	}
	if res.HasMoreBefore || res.HasMoreAfter || res.NextBefore != "" || res.NextAfter != "" {
		t.Fatalf("short session continuation = (%v,%q,%v,%q), want all clear", res.HasMoreBefore, res.NextBefore, res.HasMoreAfter, res.NextAfter)
	}
}

// TestTranscriptBudgetMeasuresJSONSerialization pins the budget's measure
// itself: what accumulates is the block's JSON serialization length (the
// bytes the page actually transfers), and it is deterministic across calls.
func TestTranscriptBudgetMeasuresJSONSerialization(t *testing.T) {
	b := pageBlock("measure", 3, 17)
	first := marshaledBlockSize(b)
	if first <= 0 {
		t.Fatalf("marshaledBlockSize = %d, want positive", first)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if first != len(data) {
		t.Fatalf("marshaledBlockSize = %d, want len(json.Marshal) = %d", first, len(data))
	}
	for i := 0; i < 3; i++ {
		if again := marshaledBlockSize(pageBlock("measure", 3, 17)); again != first {
			t.Fatalf("measure is not deterministic: %d then %d", first, again)
		}
	}
}
