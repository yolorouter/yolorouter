// frontend/src/utils/turnEvents.ts
//
// Read-side event classification for the session replay's conversation
// model. The backend classifies every message a request newly contributed
// AT WRITE TIME into the persisted turn store (internal/agentturns'
// classify.go: user_input / tool_result / injected), but the transcript
// wire contract delivers the classified rows as one flat, undifferentiated
// `added` array — same objects, same order (user_input + tool_result +
// injected concatenated back is exactly the wire array, the store's
// losslessness contract). Since the block-level split landed, those wire
// elements are per-block synthetics: a user message that mixed IDE context
// with human words arrives as separate injected and user elements, and a
// resent duplicate input never arrives at all (folded at write time) — so
// the mirror grades exactly what the axis shows. This module re-derives
// the per-message class on the client by MIRRORING the backend's rules
// one-to-one, so the replay page can grade its rendering the way the turn
// store grades its rows:
//
//   user_input   the only message class that renders a user bubble on the
//                conversation axis
//   tool_result  renders as a compact result card, an attachment of the
//                turn rather than a participant in the conversation
//   injected     machine-injected context (system reminders, skill
//                listings, command echoes, caveats) — collapsed off the
//                axis behind one "injected context (N sections)" line
//
// The mirror is deliberate duplication of a SMALL, conservative rule table
// (a four-marker prefix list plus two structural checks). If the backend
// table grows, this file grows with it — the table-driven tests below pin
// the same cases classify.go's own tests pin.
//
// Pure functions, no Vue/i18n imports — table-testable with zero DOM.

import { toolResultText, translateOneMessage, type ChatMessage, type TranslatedBody } from './messageTranslator'

/** The turn-store event classes a wire `added` message can carry. */
export type AddedEventClass = 'user_input' | 'tool_result' | 'injected'

// Leading markers of machine-injected context in caller-side user
// messages — the exact table internal/agentturns/classify.go applies at
// write time. A message whose text begins with one of these is a machine
// product (Claude Code's system-reminder wrappers, command echoes, the
// local-mode caveat, an IDE-coupled client's editor context — matched by
// the bare '<ide_' stem every observed wrapper shares), not a human's
// input. The marker must LEAD the trimmed text: whole-message wrappers
// only, mid-text occurrences are ordinary quoted content.
const INJECTED_CONTENT_PREFIXES = ['<system-reminder', '<command-name', '<command-message', 'Caveat:', '<ide_']

// Text of a user message for marker inspection: string content verbatim,
// a blocks array reduced to its text blocks joined by newlines — the same
// extraction classify.go's classifyUserMessage runs before prefix-matching.
function userTextForClassification(content: unknown): string {
  if (typeof content === 'string') return content
  if (!Array.isArray(content)) return ''
  const texts: string[] = []
  for (const block of content) {
    if (typeof block === 'object' && block !== null && !Array.isArray(block)) {
      const b = block as Record<string, unknown>
      if (b.type === 'text' && typeof b.text === 'string') texts.push(b.text)
    }
  }
  return texts.join('\n')
}

function classifyTextContent(text: string): AddedEventClass {
  const trimmed = text.replace(/^[ \t\r\n]+/, '')
  for (const prefix of INJECTED_CONTENT_PREFIXES) {
    if (trimmed.startsWith(prefix)) return 'injected'
  }
  return 'user_input'
}

// Mirror of classify.go's ClassifyAddedMessage — same arms, same order:
// role "tool" (the OpenAI dialect's tool results) → tool_result; user-role
// content with any tool_result block → tool_result (checked BEFORE the
// text inspection: Anthropic delivers tool results inside user messages);
// user text leading with an injection marker → injected; everything else
// (including non-objects and unrecognized roles — the catch-all keeps the
// classification total) stays on the conversation axis as user_input.
export function classifyAddedMessage(m: unknown): AddedEventClass {
  if (typeof m !== 'object' || m === null || Array.isArray(m)) return 'user_input'
  const obj = m as Record<string, unknown>
  const role = typeof obj.role === 'string' ? obj.role : ''
  if (role === 'tool') return 'tool_result'
  if (role !== 'user') return 'user_input'
  const content = obj.content
  if (Array.isArray(content)) {
    let hasToolResult = false
    for (const block of content) {
      if (typeof block === 'object' && block !== null && !Array.isArray(block)) {
        const b = block as Record<string, unknown>
        if (b.type === 'tool_result') hasToolResult = true
      }
    }
    if (hasToolResult) return 'tool_result'
    return classifyTextContent(userTextForClassification(content))
  }
  if (typeof content === 'string') return classifyTextContent(content)
  return 'user_input'
}

/** One extracted tool-result payload: which call it answers, and its content. */
export interface ToolResultPayload {
  /** The wire id of the call this result answers (Anthropic tool_use_id /
   *  OpenAI tool_call_id); '' when the wire carried none. */
  toolUseId: string
  /** The result's content, verbatim from the wire (string or blocks). */
  content: unknown
}

// Extract every tool-result payload one classified tool_result message
// carries, in wire order: the Anthropic dialect nests tool_result blocks
// inside a user message's content (parallel calls arrive as sibling
// blocks); the OpenAI dialect delivers one role:"tool" message per call
// with its output as the message content.
export function extractToolResults(m: unknown): ToolResultPayload[] {
  if (typeof m !== 'object' || m === null || Array.isArray(m)) return []
  const obj = m as Record<string, unknown>
  const role = typeof obj.role === 'string' ? obj.role : ''
  if (role === 'tool') {
    const id = typeof obj.tool_call_id === 'string' ? obj.tool_call_id : ''
    return [{ toolUseId: id, content: obj.content }]
  }
  if (!Array.isArray(obj.content)) return []
  const out: ToolResultPayload[] = []
  for (const block of obj.content) {
    if (typeof block !== 'object' || block === null || Array.isArray(block)) continue
    const b = block as Record<string, unknown>
    if (b.type !== 'tool_result') continue
    out.push({ toolUseId: typeof b.tool_use_id === 'string' ? b.tool_use_id : '', content: b.content })
  }
  return out
}

/** The render-graded segments of one replay block's `added` slice. */
export type AddedSegment =
  /** A human input — the only bubble source on the conversation axis. */
  | { kind: 'user'; message: ChatMessage }
  /** One tool result — renders as a compact card, not a bubble. */
  | { kind: 'tool_result'; toolUseId: string; content: unknown }
  /** One contiguous run of machine-injected context, collapsed to a line. */
  | { kind: 'injected'; count: number; texts: string[] }

// Grade one block's added slice into render segments, preserving wire
// order: user inputs become translated chat messages, tool results become
// per-payload cards (a parallel-call message yields sibling cards), and
// consecutive injected messages fold into ONE collapsed run whose count is
// the run's message total. An empty / all-noise slice yields [] — the
// block then renders only its assistant half, never a filler bubble.
export function buildAddedSegments(added: unknown[]): AddedSegment[] {
  const segments: AddedSegment[] = []
  for (const m of added) {
    const cls = classifyAddedMessage(m)
    if (cls === 'user_input') {
      const message = translateOneMessage(m)
      // Nothing renderable (an empty message, a non-record stray) renders
      // nothing — an empty bubble is exactly what this rework removes.
      if (message && (message.parts.length > 0 || message.text !== '')) {
        segments.push({ kind: 'user', message })
      }
    } else if (cls === 'tool_result') {
      const payloads = extractToolResults(m)
      if (payloads.length > 0) {
        for (const p of payloads) segments.push({ kind: 'tool_result', toolUseId: p.toolUseId, content: p.content })
      } else {
        // Classified by structure but carrying no tool_result payload
        // (a shape the table did not meet): keep the content visible as a
        // result card rather than dropping it — same never-lose-content
        // rule the backend's catch-all applies.
        segments.push({ kind: 'tool_result', toolUseId: '', content: (m as Record<string, unknown>)?.content ?? '' })
      }
    } else {
      const text = userTextForClassification((m as Record<string, unknown>)?.content)
      const last = segments[segments.length - 1]
      if (last && last.kind === 'injected') {
        last.count += 1
        last.texts.push(text)
      } else {
        segments.push({ kind: 'injected', count: 1, texts: [text] })
      }
    }
  }
  return segments
}

// The id → tool-name index over translated bodies: every tool_use /
// tool_calls part the loaded window's assistant replies carry contributes
// its id and name, so a LATER turn's tool_result card can name the tool
// that produced it (the dialects ship only ids on the result side). Grows
// reactively with the loaded window — a result whose calling turn is not
// loaded yet renders with the bare card title and fills in when it loads.
export function collectToolNames(body: TranslatedBody, into: Map<string, string>): void {
  if (body.kind !== 'messages') return
  for (const message of body.messages) {
    for (const part of message.parts) {
      if (part.type === 'tool' && part.id && part.name) into.set(part.id, part.name)
    }
  }
}

/** The readable output text of a tool-result payload (shared extraction). */
export function toolResultDetail(content: unknown): string {
  return toolResultText(content)
}

/** First non-empty line of a tool result's output, capped — the card's preview. */
export function firstPreviewLine(text: string, cap = 100): string {
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (trimmed !== '') return trimmed.length > cap ? trimmed.slice(0, cap) + '…' : trimmed
  }
  return ''
}
