// frontend/src/views/agent-sessions/replayPaging.test.ts
//
// Table coverage for the replay page's pure paging helpers:
//
//   - anchorRequestId: #req-<id> parsing (the deep-link entry);
//   - windowFromPage / mergeWindow: the loaded window's cursor bookkeeping
//     — seeding from a page, prepending an older page (exclusive cursor:
//     nothing overlaps), appending a newer page (inclusive cursor: the
//     anchor block resends and must dedupe), and the untouched opposite
//     direction's flags on each merge;
//   - prependCompensation (TC-10's pure leg): the upscroll viewport
//     compensation is exactly the before/after scrollHeight difference.
import { describe, expect, it } from 'vitest'
import type { AgentSessionReplayBlock, AgentSessionTranscript } from '../../api/agentSessions'
import { anchorRequestId, mergeWindow, prependCompensation, windowFromPage, type TranscriptWindow } from './replayPaging'

function block(id: string): AgentSessionReplayBlock {
  return {
    request_id: id,
    created_at: '2026-10-10T00:00:00Z',
    model_name: 'm',
    status_class: 'success',
    is_stream: false,
    input_tokens: 1,
    output_tokens: 1,
    duration_ms: 1,
    provider_name: 'p',
    added: [],
    assistant: { source: 'none', truncated: false, raw: null },
    added_truncated: false,
  }
}

function page(blocks: string[], hasMoreBefore: boolean, nextBefore: string, hasMoreAfter: boolean, nextAfter: string): AgentSessionTranscript {
  return {
    session: {
      agent_session_id: 's',
      agent_client: 'claude-code',
      request_count: blocks.length,
      success_count: blocks.length,
      input_tokens: 0,
      output_tokens: 0,
      known_cost_micros: 0,
      unknown_cost_count: 0,
      first_seen_at: '2026-10-10T00:00:00Z',
      last_seen_at: '2026-10-10T00:00:00Z',
    },
    system: [],
    blocks: blocks.map(block),
    has_more_before: hasMoreBefore,
    next_before: nextBefore,
    has_more_after: hasMoreAfter,
    next_after: nextAfter,
  }
}

describe('anchorRequestId', () => {
  it.each([
    ['#req-abc123', 'abc123'],
    ['#req-b14', 'b14'],
    // Not a turn anchor: no hash, a foreign hash, or the bare prefix.
    ['', null],
    ['#settings', null],
    ['#req-', null],
  ])('%j → %j', (hash, expected) => {
    expect(anchorRequestId(hash)).toBe(expected)
  })
})

describe('windowFromPage', () => {
  it('seeds both cursor pairs from the page', () => {
    const w = windowFromPage(page(['b1', 'b2'], true, 'b1', false, ''))
    expect(w.blocks.map((b) => b.request_id)).toEqual(['b1', 'b2'])
    expect(w.hasMoreBefore).toBe(true)
    expect(w.nextBefore).toBe('b1')
    expect(w.hasMoreAfter).toBe(false)
    expect(w.nextAfter).toBe('')
  })
})

describe('mergeWindow', () => {
  const seeded = (ids: string[], hasMoreBefore = false, nextBefore = '', hasMoreAfter = false, nextAfter = ''): TranscriptWindow => ({
    ...windowFromPage(page(ids, hasMoreBefore, nextBefore, hasMoreAfter, nextAfter)),
  })

  it("side 'older' prepends the page's blocks and takes the page's older cursor", () => {
    const w = mergeWindow(
      seeded(['b4', 'b5'], true, 'b4', false, ''),
      page(['b2', 'b3'], true, 'b2', true, 'b3'),
      'older',
    )
    expect(w.blocks.map((b) => b.request_id)).toEqual(['b2', 'b3', 'b4', 'b5'])
    // The older cursor passes to the page's own; the newer direction
    // cannot have changed (the window's newest block did not move).
    expect(w.hasMoreBefore).toBe(true)
    expect(w.nextBefore).toBe('b2')
    expect(w.hasMoreAfter).toBe(false)
    expect(w.nextAfter).toBe('')
  })

  it("side 'newer' appends the page's blocks and takes the page's newer cursor; the resent anchor block dedupes", () => {
    const w = mergeWindow(
      seeded(['b1', 'b2'], true, 'b1', true, 'b2'),
      // The inclusive after cursor resends b2 as the page's first block.
      page(['b2', 'b3', 'b4'], false, '', true, 'b4'),
      'newer',
    )
    expect(w.blocks.map((b) => b.request_id)).toEqual(['b1', 'b2', 'b3', 'b4'])
    expect(w.hasMoreAfter).toBe(true)
    expect(w.nextAfter).toBe('b4')
    // The older cursor stays the window's own.
    expect(w.hasMoreBefore).toBe(true)
    expect(w.nextBefore).toBe('b1')
  })

  it('drops blocks the window already holds, whichever side they arrive on', () => {
    const w = mergeWindow(
      seeded(['b1', 'b2', 'b3']),
      page(['b2', 'b3'], false, '', false, ''),
      'newer',
    )
    expect(w.blocks.map((b) => b.request_id)).toEqual(['b1', 'b2', 'b3'])
  })

  it('an empty page (a stale cursor) merges to no change and clears the direction it answered', () => {
    const w = mergeWindow(
      seeded(['b1'], true, 'b1', false, ''),
      page([], false, '', false, ''),
      'older',
    )
    expect(w.blocks.map((b) => b.request_id)).toEqual(['b1'])
    expect(w.hasMoreBefore).toBe(false)
    expect(w.nextBefore).toBe('')
  })
})

describe('prependCompensation (TC-10 pure leg)', () => {
  it.each([
    // No growth, nothing to compensate.
    [1000, 1000, 0],
    // A 250px insertion above the viewport scrolls down by exactly 250px.
    [1000, 1250, 250],
    // A tall page of history: the full difference, however large.
    [4000, 9000, 5000],
    // Content above shrank instead of growing: symmetric negative.
    [1250, 1000, -250],
  ])('heightBefore=%i, heightAfter=%i → %i', (before, after, expected) => {
    expect(prependCompensation(before, after)).toBe(expected)
  })
})
