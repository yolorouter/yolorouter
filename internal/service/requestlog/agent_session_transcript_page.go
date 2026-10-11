// Cursor pagination for the tool-session transcript — the output-layer
// slicer over the replay sequence. The seam is deliberate: the replay
// semantics live in internal/agentturns (whole-session pass as the
// specification, the persisted turn store as the production source), and
// this file slices ONE finished sequence for transfer. Its input is the
// transcriptBlockSequence abstraction — a length, an id lookup, and a
// materialize-on-demand block accessor — so the slice-backed call (the
// table tests) and the turns-backed reader (fetching only the window the
// walk touches) run the SAME walk: because every taken block is fully
// materialized before the page is returned, a page-tail block always
// carries its resend attribution — there is no cross-page state to lose.
package requestlog

import "encoding/json"

import "github.com/yolorouter/yolorouter/internal/agentturns"

// Transcript pagination constants. The count defaults mirror the
// page/page_size convention every other paginated admin endpoint holds
// (default 20, max 200); the byte budget is this endpoint's own — 1.5 MiB of
// accumulated JSON-serialized block bytes, so one huge request body cannot
// blow a page past what a first screen should transfer.
const (
	transcriptDefaultLimit = 20
	transcriptMaxLimit     = 200
	transcriptByteBudget   = 1572864 // 1.5 MiB
)

// AgentSessionTranscriptPage is the cursor-pagination request for the
// transcript endpoint. Exactly one direction applies per call:
//
//   - Before (exclusive): the blocks strictly OLDER than the cursor block —
//     the upscroll continuation. The cursor block itself is not re-sent.
//   - After (inclusive): the blocks from the cursor block onward — the
//     anchor deep-link entry and the toward-newer continuation share it.
//   - Neither: the latest blocks (the first screen lands on the newest turn).
//
// A cursor that matches no block in the session is a bad cursor and yields an
// empty page with both has_more flags false (lenient by design: a stale deep
// link must not explode). ByteBudget is a test seam — zero means the shipped
// 1.5 MiB constant.
type AgentSessionTranscriptPage struct {
	Limit      int
	Before     string
	After      string
	ByteBudget int
}

// AgentSessionTranscriptPageResult is one sliced page plus the direction-wise
// continuation metadata. NextBefore/NextAfter are empty exactly when their
// has_more flag is false, so "no more in that direction" and "no cursor to
// continue with" can never disagree.
type AgentSessionTranscriptPageResult struct {
	Blocks        []agentturns.AgentSessionReplayBlock
	HasMoreBefore bool
	NextBefore    string
	HasMoreAfter  bool
	NextAfter     string
}

// transcriptBlockSequence is the slicer's view of one session's block
// sequence: how many blocks exist, the request id of block i, and the
// block itself. The block accessor may materialize lazily (the turns-backed
// reader fetches a request's turns and body row only when the walk reaches
// it); it must be memoizing, because the walk can ask for the same index
// twice (the take that measured it, then the cursor that names it).
type transcriptBlockSequence struct {
	length  int
	idAt    func(i int) string
	blockAt func(i int) agentturns.AgentSessionReplayBlock
}

// sliceBackedSequence wraps an in-memory slice: the table-tested shape.
func sliceBackedSequence(blocks []agentturns.AgentSessionReplayBlock) transcriptBlockSequence {
	return transcriptBlockSequence{
		length:  len(blocks),
		idAt:    func(i int) string { return blocks[i].RequestID },
		blockAt: func(i int) agentturns.AgentSessionReplayBlock { return blocks[i] },
	}
}

// SliceAgentSessionTranscriptBlocks slices the fully assembled chronological
// block sequence into one page — the slice-backed form of the walk below,
// kept as the table tests' entry point.
func SliceAgentSessionTranscriptBlocks(blocks []agentturns.AgentSessionReplayBlock, page AgentSessionTranscriptPage) AgentSessionTranscriptPageResult {
	return sliceTranscriptBlocks(sliceBackedSequence(blocks), page)
}

// sliceTranscriptBlocks walks one block sequence into a page. Every page is
// anchored at one end of its window and extends in one direction, capped by
// whichever comes first:
//
//   - the block-count limit (normalized here: invalid, non-positive, or
//     over-200 values fall back to the default 20 — the same silent-fallback
//     precedent the page/page_size endpoints set);
//   - the byte budget: blocks are appended one at a time in extension order,
//     accumulating their JSON serialization byte counts; a block whose
//     append would push the cumulative to >= the budget is NOT appended
//     (already-included blocks are never rolled back);
//   - the single-oversized-block exception: the FIRST block of a page is
//     always taken, even when it alone reaches the budget — a page may then
//     exceed the budget with exactly that one block, because the contract
//     refuses neither to serve nor to cut a single block.
//
// Returned blocks keep chronological (oldest→newest) order.
//
// The toward-newer continuation always advances: next_after normally names
// the page's newest block (the next call re-anchors on it inclusively), but
// when the after page carries nothing beyond its own anchor block — the
// limit or the byte budget stopped the walk right at the anchor — next_after
// names the first block beyond the page instead, so re-anchoring can never
// return the same single-block page forever.
//
// The walk only ever materializes the blocks it takes (plus, at most, the
// first block it refuses — never, in fact: a refused block ends the walk
// before its cursor is needed) and resolves cursors by id alone, so a
// lazy-backed sequence pays for exactly the page it returns.
func sliceTranscriptBlocks(seq transcriptBlockSequence, page AgentSessionTranscriptPage) AgentSessionTranscriptPageResult {
	limit := normalizeTranscriptLimit(page.Limit)
	budget := page.ByteBudget
	if budget <= 0 {
		budget = transcriptByteBudget
	}

	// Resolve the cursor into a half-open window [ws, we) plus the direction
	// the page extends in. The no-cursor call is the newest-anchored walk
	// over the whole sequence (first screen = latest turns).
	var ws, we int
	towardOlder := true
	switch {
	case page.Before != "":
		idx := transcriptBlockIndex(seq, page.Before)
		if idx < 0 {
			return emptyTranscriptPage()
		}
		ws, we = 0, idx
	case page.After != "":
		idx := transcriptBlockIndex(seq, page.After)
		if idx < 0 {
			return emptyTranscriptPage()
		}
		ws, we = idx, seq.length
		towardOlder = false
	default:
		ws, we = 0, seq.length
	}
	if ws >= we {
		// A known cursor whose window is empty (e.g. before=<first block>)
		// reads the same as a bad cursor: nothing to add, nothing more.
		return emptyTranscriptPage()
	}

	// Walk from the anchor extending in the page direction, applying the
	// count/budget caps above. A down-walk (towardOlder) prepends, so the
	// page keeps the blocks nearest the anchor; an up-walk appends. take
	// reports whether the block was taken — a refused block ends the walk
	// (a later smaller one must not sneak in behind it).
	out := make([]agentturns.AgentSessionReplayBlock, 0, limit)
	cumBytes := 0
	take := func(i int) bool {
		b := seq.blockAt(i)
		size := marshaledBlockSize(b)
		if len(out) > 0 && cumBytes+size >= budget {
			return false // stop BEFORE this block: cumulative would reach the budget
		}
		if towardOlder {
			out = append([]agentturns.AgentSessionReplayBlock{b}, out...)
		} else {
			out = append(out, b)
		}
		cumBytes += size
		return true
	}
	if towardOlder {
		for i := we - 1; i >= ws; i-- {
			if len(out) == limit || !take(i) {
				break
			}
			if cumBytes >= budget {
				break // the single oversized block occupies its own page
			}
		}
	} else {
		for i := ws; i < we; i++ {
			if len(out) == limit || !take(i) {
				break
			}
			if cumBytes >= budget {
				break // the single oversized block occupies its own page
			}
		}
	}
	if len(out) == 0 {
		return emptyTranscriptPage()
	}

	// The page is contiguous: derive its bounds for the direction-wise
	// has_more flags. next_before names the page's OLDEST block (upscroll
	// continues strictly before it, so the before walk always shrinks its
	// window); next_after names the page's NEWEST block (the toward-newer
	// continuation re-anchors on it, inclusive) — except in the one case
	// where that would stall, handled below.
	s, e := ws, we
	if towardOlder {
		s = we - len(out)
	} else {
		e = ws + len(out)
	}
	res := AgentSessionTranscriptPageResult{
		Blocks:        out,
		HasMoreBefore: s > 0,
		HasMoreAfter:  e < seq.length,
	}
	if res.HasMoreBefore {
		res.NextBefore = seq.idAt(s)
	}
	if res.HasMoreAfter {
		if !towardOlder && e-1 == ws {
			// The after page is nothing but its own anchor: the limit or the
			// byte budget stopped the walk before a second block fit (e.g. a
			// normal anchor whose successor alone would blow the budget).
			// Inclusive re-anchoring on the anchor itself would return this
			// same page forever — zero progress — so the cursor names the
			// first block BEYOND the page. That block was never delivered
			// (the walk refused or never reached it), so the continuation
			// loses nothing, and the walk still ends on a page whose window
			// it exhausts.
			res.NextAfter = seq.idAt(e)
		} else {
			res.NextAfter = seq.idAt(e - 1)
		}
	}
	return res
}

// normalizeTranscriptLimit applies the limit boundary: any limit that is not
// a positive integer up to the max falls back to the default.
func normalizeTranscriptLimit(limit int) int {
	if limit <= 0 || limit > transcriptMaxLimit {
		return transcriptDefaultLimit
	}
	return limit
}

// transcriptBlockIndex finds the cursor block's index by request id, or -1
// when the cursor names no block of this session (a bad cursor).
func transcriptBlockIndex(seq transcriptBlockSequence, requestID string) int {
	for i := 0; i < seq.length; i++ {
		if seq.idAt(i) == requestID {
			return i
		}
	}
	return -1
}

// emptyTranscriptPage is the shared empty answer: no blocks, both has_more
// flags false, no continuation cursors. It covers both bad cursors (lenient
// deep links) and legitimate-but-empty windows.
func emptyTranscriptPage() AgentSessionTranscriptPageResult {
	return AgentSessionTranscriptPageResult{Blocks: []agentturns.AgentSessionReplayBlock{}}
}

// marshaledBlockSize is the deterministic per-block size the budget
// accumulates: the byte length of the block's JSON serialization. encoding/
// json sorts map keys, so the same block always measures the same — the
// budget never depends on map iteration order.
func marshaledBlockSize(b agentturns.AgentSessionReplayBlock) int {
	data, err := json.Marshal(b)
	if err != nil {
		// Unreachable for this shape (no channels/functions anywhere in it);
		// a zero size would only ever under-count, never over.
		return 0
	}
	return len(data)
}
