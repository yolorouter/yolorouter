// frontend/src/views/agent-sessions/replayPaging.ts
//
// Pure helpers for the replay page's lazy loading — no DOM, no Vue, so the
// cursor bookkeeping and the scroll arithmetic stay table-testable. The
// page wires them to the transcript client, the sentinels, and the scroll
// container.
import type { AgentSessionReplayBlock, AgentSessionTranscript } from '../../api/agentSessions'

// The loaded window: the blocks currently rendered (chronological order)
// plus the continuation cursors of whichever page last extended each side.
// One cursor pair per side is enough because the loaded set is always one
// contiguous run — pages only ever extend an end, they never punch a hole
// in the middle.
export interface TranscriptWindow {
  blocks: AgentSessionReplayBlock[]
  hasMoreBefore: boolean
  nextBefore: string
  hasMoreAfter: boolean
  nextAfter: string
}

// windowFromPage seeds a window from a freshly fetched page (the initial
// load and every re-anchor): both cursor pairs come straight from the
// response.
export function windowFromPage(page: AgentSessionTranscript): TranscriptWindow {
  return {
    blocks: [...page.blocks],
    hasMoreBefore: page.has_more_before,
    nextBefore: page.next_before,
    hasMoreAfter: page.has_more_after,
    nextAfter: page.next_after,
  }
}

// anchorRequestId extracts the deep-linked turn from a route hash:
// #req-<requestId> names the block to land on; anything else (no hash, a
// foreign hash) anchors nothing.
export function anchorRequestId(hash: string): string | null {
  if (!hash.startsWith('#req-')) return null
  const id = hash.slice('#req-'.length)
  return id.length > 0 ? id : null
}

// mergeWindow folds one fetched page into the loaded window. `side` names
// the end the page extends:
//   - 'older': the page answered before=<window's oldest>, so its blocks
//     prepend and the older-direction cursor passes to the page's own (the
//     newer direction cannot have changed — the window's newest block did
//     not move);
//   - 'newer': the page answered after=<window's newest>, so its blocks
//     append and the newer-direction cursor passes to the page's own.
// Blocks already in the window are dropped — the inclusive after cursor
// resends the anchor block by design, and a re-fired direction must never
// duplicate a turn — keeping the window one contiguous, duplicate-free run.
export function mergeWindow(current: TranscriptWindow, page: AgentSessionTranscript, side: 'older' | 'newer'): TranscriptWindow {
  const seen = new Set(current.blocks.map((b) => b.request_id))
  const incoming = page.blocks.filter((b) => !seen.has(b.request_id))
  return {
    blocks: side === 'older' ? [...incoming, ...current.blocks] : [...current.blocks, ...incoming],
    hasMoreBefore: side === 'older' ? page.has_more_before : current.hasMoreBefore,
    nextBefore: side === 'older' ? page.next_before : current.nextBefore,
    hasMoreAfter: side === 'newer' ? page.has_more_after : current.hasMoreAfter,
    nextAfter: side === 'newer' ? page.next_after : current.nextAfter,
  }
}

// prependCompensation is the scrollTop adjustment that keeps the viewport
// on the same turn after older blocks were inserted ABOVE it: the
// insertion grows the scroll container's content by exactly
// (heightAfter - heightBefore) pixels, and adding that difference to
// scrollTop leaves the pixels under the viewport — and therefore the turn
// being read — where they were. A negative difference (content above
// shrank instead of growing, say after a re-render) scrolls back up by the
// same arithmetic; the scroll container's own bounds do any clamping.
export function prependCompensation(heightBefore: number, heightAfter: number): number {
  return heightAfter - heightBefore
}
