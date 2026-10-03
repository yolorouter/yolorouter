// frontend/src/utils/messageTranslator.ts
//
// Pure-function message translator for the session-detail message flow:
// turns a captured request/response/stream body (the raw JSON text the
// request-log detail API inlines) into the unified internal chat shape
// the bubbles render, or into an explicit fallback/placeholder/truncation
// signal when the body cannot be read as chat.
//
// Three entry points, one per body side:
//   translateRequestBody  — OpenAI chat `messages[]` / Anthropic messages
//                           content blocks
//   translateResponseBody — OpenAI `choices[].message` / Anthropic
//                           top-level `content` array; empty body (a stream
//                           request's response lives on disk) → placeholder
//   translateStreamBody   — SSE increment merge: OpenAI
//                           `choices[].delta.content` sequences and Anthropic
//                           `content_block_delta.text_delta` sequences (block
//                           boundaries included) concatenated back into the
//                           full reply text
//
// Format judgment order is FIXED: Anthropic features are
// tested first (top-level `system`, content arrays that read as Anthropic
// blocks), then OpenAI (`messages` with string/array content). The two
// parsers emit the same internal structure, so a body both accept (e.g. a
// pure-text content array — `text` exists in both vocabularies) still
// renders; the order only matters for bodies carrying features of both.
//
// This module is deliberately free of Vue/i18n/component imports — the
// three matrices are pinned by table-driven vitest cases with zero DOM.

// ---------------------------------------------------------------------------
// Internal unified shape (the translator's output contract)
// ---------------------------------------------------------------------------

/** A text piece, shown verbatim. */
export interface ChatTextPart {
  type: 'text'
  text: string
}

/** An image placeholder block — the payload (often megabytes of base64) is
 *  deliberately NOT carried into the render path. */
export interface ChatImagePart {
  type: 'image'
}

/** Any other non-text part (tool_use / tool_calls / tool_result / ...),
 *  shown as a type label pill. When the block carries a payload worth
 *  reading — a tool call's arguments, a tool result's output text — the
 *  translator lifts it into `detail` and the bubble renders it as a
 *  scrollable block under the pill; without a payload it stays a pill. */
export interface ChatToolPart {
  type: 'tool'
  label: string
  detail?: string
  /** Highlight language for `detail` — set only when the payload is known
   *  to be pretty-printed JSON (a tool call's arguments), so the bubble
   *  renders it through highlight.js. Arbitrary tool output carries no
   *  lang and stays plain mono text. */
  detailLang?: 'json'
}

export type ChatPart = ChatTextPart | ChatImagePart | ChatToolPart

export interface ChatMessage {
  /** Wire role, passed through verbatim ('user' | 'assistant' | 'system' |
   *  'tool' | 'developer' | ...). '' when the source message carried none. */
  role: string
  /** Concatenation of this message's text parts — the convenience field
   *  simple single-run rendering reads instead of walking parts. */
  text: string
  parts: ChatPart[]
}

/** What the caller should render for a body. */
export type TranslatedBodyKind =
  /** Body parsed as chat — render the bubbles. */
  | 'messages'
  /** Body is empty — e.g. a stream request's response body is not recorded
   *  inline (it lives on disk); the caller shows a placeholder note. */
  | 'placeholder'
  /** Body cannot be read as chat (non-JSON, wrong shape, empty message
   *  list, unmergeable stream) — the caller shows the collapsible
   *  formatted-JSON fallback view. */
  | 'fallback'

export interface TranslatedBody {
  kind: TranslatedBodyKind
  /** Only meaningful for kind === 'messages'; [] otherwise. */
  messages: ChatMessage[]
  /** True when the backend's 1 MiB inline truncation marker was detected
   *  in the body — the caller shows the "content too long, truncated" hint
   *  next to whatever it renders. */
  truncated: boolean
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
}

// The request-log service appends this human-readable suffix whenever it
// inlines more than 1 MiB of a body (internal/service/requestlog's
// inlineTruncationMarker: "\n\n… [truncated: showing first %d of %d
// bytes]"). A body carrying it is no longer valid JSON by construction,
// so every entry point checks for it BEFORE parsing and reports
// truncated=true alongside the fallback signal. \s* tolerates a trailing
// newline some transports add.
const INLINE_TRUNCATION_MARKER_RE = /… \[truncated: showing first \d+ of \d+ bytes\]\s*$/

function hasInlineTruncationMarker(raw: string): boolean {
  return INLINE_TRUNCATION_MARKER_RE.test(raw)
}

// Strip a leading BOM (same convention as the request-log stream viewer):
// the gateway reads bytes, and a UTF-8 BOM at the start would defeat every
// prefix check below.
function stripBOM(s: string): string {
  return s.charCodeAt(0) === 0xfeff ? s.slice(1) : s
}

const FALLBACK: TranslatedBody = { kind: 'fallback', messages: [], truncated: false }
const PLACEHOLDER: TranslatedBody = { kind: 'placeholder', messages: [], truncated: false }

function truncatedFallback(): TranslatedBody {
  return { kind: 'fallback', messages: [], truncated: true }
}

// Label for a non-text part: the raw wire type, plus the tool name when the
// block carries one ("tool_use: Bash"). Technical strings, not localized
// copy — the bubble component wraps them with its own i18n tag.
function toolLabel(type: string, name: unknown): string {
  return typeof name === 'string' && name ? `${type}: ${name}` : type
}

// A tool payload worth showing: JSON (object or JSON-string) pretty-printed,
// anything else as-is; empty payload → '' (no detail block). The lang marks
// output that is known to be JSON, so the bubble highlights it.
const TOOL_DETAIL_CAP = 4000
interface ToolPayload {
  text: string
  lang?: 'json'
}
function toolDetail(v: unknown): ToolPayload {
  let out = ''
  let json = false
  if (typeof v === 'string') out = v.trim()
  else {
    try {
      out = JSON.stringify(v, null, 2) ?? ''
      json = true
    } catch {
      out = ''
    }
  }
  // A trivially empty payload ("{}" / "null") has nothing to read.
  if (out === '' || out === '{}' || out === 'null') return { text: '' }
  // A JSON-string payload (OpenAI tool_calls arguments arrive as a string)
  // reads better pretty-printed; unparseable strings stay verbatim.
  if (typeof v === 'string') {
    try {
      out = JSON.stringify(JSON.parse(v), null, 2)
      json = true
    } catch {
      /* keep verbatim */
    }
  }
  if (out.length > TOOL_DETAIL_CAP) out = out.slice(0, TOOL_DETAIL_CAP) + '\n… [truncated]'
  return json ? { text: out, lang: 'json' } : { text: out }
}

// The one constructor for tool parts: a payload worth reading becomes a
// pill with a detail block (highlighted when it is JSON), anything else
// stays a bare pill.
function toolPart(label: string, payload: ToolPayload): ChatToolPart {
  if (!payload.text) return { type: 'tool', label }
  if (payload.lang) return { type: 'tool', label, detail: payload.text, detailLang: payload.lang }
  return { type: 'tool', label, detail: payload.text }
}

// A tool_result's readable output: string content verbatim, a blocks array
// reduced to its text blocks' texts, anything else no detail.
function toolResultText(content: unknown): string {
  if (typeof content === 'string') return content
  if (!Array.isArray(content)) return ''
  const texts: string[] = []
  for (const el of content) {
    if (isRecord(el) && el.type === 'text' && typeof el.text === 'string') texts.push(el.text)
  }
  return texts.join('\n')
}

function makeMessage(role: string, parts: ChatPart[]): ChatMessage {
  return {
    role,
    text: parts
      .filter((p): p is ChatTextPart => p.type === 'text')
      .map((p) => p.text)
      .join(''),
    parts,
  }
}

function emptyAssistantMessage(): ChatMessage {
  return { role: 'assistant', text: '', parts: [] }
}

// ---------------------------------------------------------------------------
// Anthropic content blocks / OpenAI content parts → ChatPart[]
// ---------------------------------------------------------------------------

// Part types that exist ONLY in the OpenAI chat-content vocabulary. Their
// presence in a content array rules out the Anthropic reading (the
// Anthropic block vocabulary has no such types), which is what keeps a
// normal OpenAI image request on the OpenAI path — with the fixed
// Anthropic-first order, the blocks check below must skip past such arrays
// instead of claiming them and degrading the image placeholder to a bare
// type label.
const OPENAI_ONLY_PART_TYPES = new Set(['image_url', 'audio', 'input_audio', 'file', 'input_file'])

// A content array reads as Anthropic blocks when it is a non-empty array
// of typed records free of OpenAI-only part types. Pure-text arrays match
// BOTH vocabularies ('text' is shared) — the fixed order sends them down
// the Anthropic path first, and since both parsers emit the same internal
// structure the ambiguity is harmless by design.
function isBlocksArray(content: unknown): boolean {
  if (!Array.isArray(content) || content.length === 0) return false
  for (const el of content) {
    if (!isRecord(el)) return false
    const type = typeof el.type === 'string' ? el.type : ''
    if (OPENAI_ONLY_PART_TYPES.has(type)) return false
  }
  return true
}

// One element of an Anthropic-style blocks array → part. Known carriers:
// text (verbatim), image (placeholder). Everything else — tool_use,
// tool_result, thinking, document, ... — is a non-text part and shows its
// type (plus the tool name for tool_use).
function blockToPart(block: Record<string, unknown>, toolNames?: Map<string, string>): ChatPart {
  const type = typeof block.type === 'string' ? block.type : ''
  if (type === 'text') {
    return { type: 'text', text: typeof block.text === 'string' ? block.text : '' }
  }
  if (type === 'image') {
    return { type: 'image' }
  }
  if (type === 'tool_use') {
    if (toolNames && typeof block.id === 'string') {
      const name = typeof block.name === 'string' ? block.name : ''
      if (name) toolNames.set(block.id, name)
    }
    return toolPart(toolLabel(type, block.name), toolDetail(block.input))
  }
  if (type === 'tool_result') {
    // Correlate with the earlier tool_use by id, so the pill reads
    // "tool_result: Bash" instead of a bare type name.
    const id = typeof block.tool_use_id === 'string' ? block.tool_use_id : ''
    const name = (id && toolNames?.get(id)) || ''
    return toolPart(toolLabel(type, name), toolDetail(toolResultText(block.content)))
  }
  return { type: 'tool', label: toolLabel(type || 'unknown', block.name) }
}

// One element of an OpenAI-style content-parts array → part. text is
// verbatim, image_url is the placeholder, anything else shows its type.
function openAIPartToPart(part: Record<string, unknown>): ChatPart {
  const type = typeof part.type === 'string' ? part.type : ''
  if (type === 'text') {
    return { type: 'text', text: typeof part.text === 'string' ? part.text : '' }
  }
  if (type === 'image_url') {
    return { type: 'image' }
  }
  return { type: 'tool', label: type || 'unknown' }
}

// Content of either vocabulary, one message at a time. Strings display
// directly; blocks/parts arrays map element-wise; any other content shape
// (null, numbers, ...) contributes no parts — the message still renders as
// an empty bubble instead of failing the whole body.
function contentToParts(content: unknown, blocksStyle: boolean, toolNames?: Map<string, string>): ChatPart[] {
  if (typeof content === 'string') {
    return content === '' ? [] : [{ type: 'text', text: content }]
  }
  if (!Array.isArray(content)) return []
  return content.filter(isRecord).map((el) => (blocksStyle ? blockToPart(el, toolNames) : openAIPartToPart(el)))
}

// ---------------------------------------------------------------------------
// OpenAI Responses dialect (/v1/responses — codex's main wire format)
// ---------------------------------------------------------------------------

// A Responses-API request body: no chat `messages`, an `input` list of
// typed items, and typically `instructions` carrying the system prompt.
// Embeddings-shaped bodies (`input` of plain strings/numbers, never typed
// records, no instructions) must NOT match — they stay fallback.
function looksLikeResponsesRequest(body: Record<string, unknown>): boolean {
  if (Array.isArray(body.messages)) return false
  if (!Array.isArray(body.input)) return false
  if (typeof body.instructions === 'string' && body.input.length === 0) return true
  return body.input.some((el) => isRecord(el) && typeof el.type === 'string')
}

// Responses content parts: the *_text carriers hold the text, image parts
// become placeholders, anything else shows its type label.
function responsesContentToParts(content: unknown): ChatPart[] {
  if (typeof content === 'string') {
    return content === '' ? [] : [{ type: 'text', text: content }]
  }
  if (!Array.isArray(content)) return []
  const parts: ChatPart[] = []
  for (const el of content) {
    if (!isRecord(el)) continue
    const type = typeof el.type === 'string' ? el.type : ''
    if ((type === 'input_text' || type === 'output_text' || type === 'text' || type === 'summary_text') && typeof el.text === 'string') {
      if (el.text !== '') parts.push({ type: 'text', text: el.text })
    } else if (type === 'input_image') {
      parts.push({ type: 'image' })
    } else {
      parts.push({ type: 'tool', label: toolLabel(type, el.name) })
    }
  }
  return parts
}

// One Responses input/output item → message. Reasoning items are the
// model's thinking and render nothing; message items carry role + content;
// *_call items are the assistant invoking a tool (type + name label);
// *_call_output items are the tool's result as a tool-role text message.
// Unknown item types stay visible as their type label.
function responsesItemToMessage(item: Record<string, unknown>): ChatMessage | null {
  const type = typeof item.type === 'string' ? item.type : ''
  if (type === 'reasoning' || type === 'item_reference') return null
  if (type === 'message' || (type === '' && (item.content !== undefined || item.role !== undefined))) {
    const role = typeof item.role === 'string' && item.role ? item.role : 'assistant'
    return makeMessage(role, responsesContentToParts(item.content))
  }
  if (type.endsWith('_call_output') || type.endsWith('_output')) {
    const out = item.output
    const parts = typeof out === 'string' ? (out === '' ? [] : [{ type: 'text' as const, text: out }]) : isRecord(out) ? responsesContentToParts(out.content) : []
    return makeMessage('tool', parts)
  }
  if (type !== '') {
    return makeMessage('assistant', [toolPart(toolLabel(type, item.name), toolDetail(item.arguments))])
  }
  return makeMessage('tool', [{ type: 'tool', label: 'item' }])
}

// ---------------------------------------------------------------------------
// Request side
// ---------------------------------------------------------------------------

// Anthropic request features, checked FIRST per the fixed judgment order:
// a top-level `system` field (string or blocks — OpenAI requests never
// have one), or any message whose content reads as Anthropic blocks.
function looksLikeAnthropicRequest(body: Record<string, unknown>): boolean {
  // Any non-empty system value (string or blocks array) is an Anthropic
  // feature; "" is treated as absent — an empty prompt claims nothing.
  if (body.system !== undefined && body.system !== '') return true
  const messages = body.messages
  if (!Array.isArray(messages)) return false
  return messages.some((m) => isRecord(m) && isBlocksArray(m.content))
}

// OpenAI request: a `messages` array whose contents are strings,
// OpenAI-part arrays, or absent (assistant messages with only tool_calls).
function looksLikeOpenAIRequest(body: Record<string, unknown>): boolean {
  return Array.isArray(body.messages) && body.messages.length > 0
}

export function translateRequestBody(raw: string): TranslatedBody {
  const cleaned = stripBOM(raw)
  if (cleaned.trim() === '') return PLACEHOLDER
  if (hasInlineTruncationMarker(cleaned)) return truncatedFallback()

  let parsed: unknown
  try {
    parsed = JSON.parse(cleaned)
  } catch {
    return FALLBACK
  }
  if (!isRecord(parsed)) return FALLBACK

  const messages: ChatMessage[] = []

  // Fixed order: Anthropic features first.
  if (looksLikeAnthropicRequest(parsed)) {
    // tool_use id → tool name, walked in order so a later tool_result
    // block can name the tool it answers.
    const toolNames = new Map<string, string>()
    // Top-level system prompt, when it carries anything, becomes the
    // leading system message (string or blocks — both accepted by the
    // wire format; an empty one is skipped rather than rendering a bare
    // empty bubble).
    if (parsed.system !== undefined && parsed.system !== '') {
      const systemParts = contentToParts(parsed.system, true, toolNames)
      if (systemParts.length > 0) messages.push(makeMessage('system', systemParts))
    }
    // messages may be absent altogether when only system matched — the
    // system message above is then the whole conversation.
    if (Array.isArray(parsed.messages)) {
      for (const m of parsed.messages) {
        if (!isRecord(m)) continue
        messages.push(makeMessage(typeof m.role === 'string' ? m.role : '', contentToParts(m.content, true, toolNames)))
      }
    }
  } else if (looksLikeOpenAIRequest(parsed)) {
    for (const m of parsed.messages as unknown[]) {
      if (!isRecord(m)) continue
      const role = typeof m.role === 'string' ? m.role : ''
      const parts = contentToParts(m.content, false)
      // Assistant tool_calls live beside content, not inside it — one
      // labeled part per call, appended in wire order.
      if (Array.isArray(m.tool_calls)) {
        for (const call of m.tool_calls) {
          if (!isRecord(call)) continue
          const fn = isRecord(call.function) ? call.function : {}
          parts.push(toolPart(toolLabel('tool_calls', fn.name), toolDetail(fn.arguments)))
        }
      }
      messages.push(makeMessage(role, parts))
    }
  } else if (looksLikeResponsesRequest(parsed)) {
    // OpenAI Responses (/v1/responses — codex's wire format): top-level
    // `instructions` is the system prompt, `input` is the conversation as
    // typed items.
    if (typeof parsed.instructions === 'string' && parsed.instructions !== '') {
      messages.push(makeMessage('system', [{ type: 'text', text: parsed.instructions }]))
    }
    for (const item of parsed.input as unknown[]) {
      if (!isRecord(item)) continue
      const m = responsesItemToMessage(item)
      if (m) messages.push(m)
    }
  } else {
    // Valid JSON but no chat shape (embeddings input, a bare object, an
    // empty messages list, ...) — nothing to render as conversation.
    return FALLBACK
  }

  return messages.length > 0 ? { kind: 'messages', messages, truncated: false } : FALLBACK
}

// ---------------------------------------------------------------------------
// Response side
// ---------------------------------------------------------------------------

export function translateResponseBody(raw: string): TranslatedBody {
  const cleaned = stripBOM(raw)
  // An empty response body means "not recorded inline" — for a stream
  // request the response lives in the on-disk capture instead. Distinct
  // from fallback: the caller shows a placeholder note, not a JSON view.
  if (cleaned.trim() === '') return PLACEHOLDER
  if (hasInlineTruncationMarker(cleaned)) return truncatedFallback()

  let parsed: unknown
  try {
    parsed = JSON.parse(cleaned)
  } catch {
    return FALLBACK
  }
  if (!isRecord(parsed)) return FALLBACK

  const messages: ChatMessage[] = []

  // Fixed order again: the Anthropic response's defining feature is the
  // top-level content array (OpenAI responses never have one — their
  // content lives under choices[].message).
  if (isBlocksArray(parsed.content)) {
    messages.push(makeMessage(typeof parsed.role === 'string' && parsed.role ? parsed.role : 'assistant', contentToParts(parsed.content, true, new Map<string, string>())))
  } else if (Array.isArray(parsed.choices) && parsed.choices.length > 0) {
    // One assistant message per choice (n>1 requests stream several; the
    // common n=1 case yields one). Choices without a message object are
    // skipped rather than failing the body.
    for (const choice of parsed.choices) {
      if (!isRecord(choice) || !isRecord(choice.message)) continue
      const msg = choice.message
      const role = typeof msg.role === 'string' && msg.role ? msg.role : 'assistant'
      const parts = contentToParts(msg.content, false)
      if (Array.isArray(msg.tool_calls)) {
        for (const call of msg.tool_calls) {
          if (!isRecord(call)) continue
          const fn = isRecord(call.function) ? call.function : {}
          parts.push(toolPart(toolLabel('tool_calls', fn.name), toolDetail(fn.arguments)))
        }
      }
      messages.push(makeMessage(role, parts))
    }
  } else if (Array.isArray(parsed.output) && parsed.output.length > 0) {
    // OpenAI Responses non-stream response: `output` is the item list.
    for (const item of parsed.output) {
      if (!isRecord(item)) continue
      const m = responsesItemToMessage(item)
      if (m) messages.push(m)
    }
  } else {
    return FALLBACK
  }

  return messages.length > 0 ? { kind: 'messages', messages, truncated: false } : FALLBACK
}

// ---------------------------------------------------------------------------
// SSE increment merge
// ---------------------------------------------------------------------------

// Anthropic stream event names on the `type` field. Recognizing the frame
// (message_start/ping/stop carry no text) keeps an all-tool_use stream on
// the messages path with an empty reply, instead of degrading to fallback.
const ANTHROPIC_STREAM_EVENT_TYPES = new Set([
  'message_start',
  'content_block_start',
  'content_block_delta',
  'content_block_stop',
  'message_delta',
  'message_stop',
  'ping',
])

export function translateStreamBody(raw: string): TranslatedBody {
  const cleaned = stripBOM(raw)
  // An empty capture merges to an empty reply — still a bubble, matching
  // the non-stream empty-content rendering.
  if (cleaned.trim() === '') {
    return { kind: 'messages', messages: [emptyAssistantMessage()], truncated: false }
  }
  const truncated = hasInlineTruncationMarker(cleaned)

  let role = 'assistant'
  let text = ''
  let sawFrame = false
  // Responses dialect: the done-frame items are the primary merge source
  // (each carries the complete message); text deltas only cover captures
  // truncated before any item completed.
  const doneItems: Array<Record<string, unknown>> = []

  for (const line of cleaned.split('\n')) {
    const trimmed = line.trim()
    if (trimmed === '' || !trimmed.startsWith('data:')) {
      // Blank separators, event:/id:/retry: headers, comments, and any
      // stray line are framing, not payloads — skipped.
      continue
    }
    // SSE allows "data:" or "data: " — the optional single space after
    // the colon is framing, not part of the value.
    const payload = (trimmed.startsWith('data: ') ? trimmed.slice(6) : trimmed.slice(5)).trim()
    if (payload === '') continue
    if (payload === '[DONE]') {
      // The OpenAI terminator is itself stream framing: a capture that
      // only ever delivered the terminator was a real (empty) OpenAI
      // stream, not an unmergeable body — it merges to an empty reply.
      sawFrame = true
      continue
    }

    let chunk: unknown
    try {
      chunk = JSON.parse(payload)
    } catch {
      // Malformed data line — skip it, never break the merge over one
      // upstream quirk (same policy as the gateway's stream forwarder).
      continue
    }
    if (!isRecord(chunk)) continue

    // OpenAI Responses event: every frame's `type` is response.*. The done
    // frames carry whole items (message with full content, function calls);
    // output_text deltas are the truncated-capture fallback. Reasoning
    // summary deltas are the model's thinking, not the reply — skipped.
    const rtype = typeof chunk.type === 'string' ? chunk.type : ''
    if (rtype.startsWith('response.')) {
      sawFrame = true
      if (rtype === 'response.output_item.done' && isRecord(chunk.item)) {
        doneItems.push(chunk.item)
      } else if (rtype === 'response.output_text.delta' && typeof chunk.delta === 'string') {
        text += chunk.delta
      }
      continue
    }

    // OpenAI-shaped chunk: choices[0].delta. The first chunk carries
    // delta.role; text arrives as delta.content pieces.
    if (Array.isArray(chunk.choices)) {
      sawFrame = true
      const first = chunk.choices[0]
      if (isRecord(first) && isRecord(first.delta)) {
        const delta = first.delta
        if (typeof delta.role === 'string' && delta.role) role = delta.role
        if (typeof delta.content === 'string') text += delta.content
      }
      continue
    }

    // Anthropic-shaped event. Only text-bearing pieces contribute:
    // content_block_start's initial text (usually '') plus every
    // text_delta. Block boundaries and indices need no bookkeeping —
    // appending in arrival order IS the block-boundary-correct merge,
    // interleaved blocks included. tool_use blocks (input_json_delta)
    // and usage frames carry no reply text and are frame-only.
    const type = typeof chunk.type === 'string' ? chunk.type : ''
    if (!ANTHROPIC_STREAM_EVENT_TYPES.has(type)) continue
    sawFrame = true
    if (type === 'content_block_start' && isRecord(chunk.content_block)) {
      const block = chunk.content_block
      if (block.type === 'text' && typeof block.text === 'string') text += block.text
    } else if (type === 'content_block_delta' && isRecord(chunk.delta)) {
      const delta = chunk.delta
      if (delta.type === 'text_delta' && typeof delta.text === 'string') text += delta.text
    }
  }

  // Nothing SSE-shaped parsed (a plain-JSON or text capture, or only
  // garbage data lines) — the fallback JSON view is the honest rendering.
  if (!sawFrame) return { kind: 'fallback', messages: [], truncated }

  // Responses dialect with completed items: the item list IS the reply
  // (assistant messages, function calls, tool outputs in wire order).
  if (doneItems.length > 0) {
    const itemMessages = doneItems
      .map(responsesItemToMessage)
      .filter((m): m is ChatMessage => m !== null)
    if (itemMessages.length > 0) {
      return { kind: 'messages', messages: itemMessages, truncated }
    }
  }

  return {
    kind: 'messages',
    messages: [
      {
        role,
        text,
        parts: text === '' ? [] : [{ type: 'text', text }],
      },
    ],
    truncated,
  }
}
