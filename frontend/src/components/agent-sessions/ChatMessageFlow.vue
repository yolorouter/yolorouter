<!-- frontend/src/components/agent-sessions/ChatMessageFlow.vue
     Renders one translated body (the messageTranslator's TranslatedBody) as
     the conversation view of the request message page:

       - kind 'messages'    chat bubbles: assistant on the right, every other
                            role on the left, each bubble carrying a
                            role-tinted chip; EVERY text part goes through the
                            markdown pipeline (renderMarkdown: markdown-it +
                            DOMPurify + highlight.js) — model replies and
                            system prompts both arrive as markdown, so no text
                            bubble is exempt; image parts render as a
                            placeholder block (the payload is deliberately not
                            carried into the DOM), and non-text parts
                            (tool_use / tool_calls / ...) show their type
                            label as a tag
       - kind 'placeholder' a muted "not recorded" note — e.g. a stream
                            request whose response body lives on disk
       - kind 'fallback'    a collapsible formatted-JSON view (the shared
                            BodyViewer), for bodies that cannot be read as
                            chat

     Long text parts (agent-tool payloads routinely carry tens of kilobytes
     of system context — enough to paint a five-figure-pixel wall) render
     clamped behind a quiet disclosure row instead, so the page stays a
     readable transcript: preview first, "show all" on demand.

     A truncation signal (the backend cut the inline body at 1 MiB) shows a
     hint line next to whatever rendering applies. -->
<template>
  <div class="msg-flow">
    <p v-if="body.truncated" class="msg-flow__truncated">{{ t('agentSessions.flowTruncated') }}</p>

    <template v-if="body.kind === 'messages'">
      <div
        v-for="(message, i) in body.messages"
        :key="i"
        class="msg-flow__row"
        :class="isAssistant(message) ? 'msg-flow__row--right' : 'msg-flow__row--left'"
      >
        <div class="msg-flow__bubble" :class="isAssistant(message) ? 'msg-flow__bubble--assistant' : 'msg-flow__bubble--other'">
          <span class="msg-flow__role" :class="`msg-flow__role--${roleKind(message.role)}`">{{ message.role || '—' }}</span>
          <template v-for="(part, j) in message.parts" :key="j">
            <template v-if="part.type === 'text'">
              <!-- Disclosure row placement: BELOW the preview while clamped
                   ("there is more"), ABOVE the text once expanded — an
                   expanded wall can be tens of thousands of pixels tall,
                   and hunting for the collapse button at its bottom is the
                   one trap this layout must not have. -->
              <div v-if="isLong(part.text) && isExpanded(i, j)" class="msg-flow__disclose">
                <button type="button" class="msg-flow__disclose-btn" @click="togglePart(i, j)">
                  {{ t('requestMessages.collapse') }}
                </button>
                <span class="msg-flow__disclose-meta">{{ t('requestMessages.partCharsNote', { n: part.text.length.toLocaleString() }) }}</span>
              </div>
              <!-- v-html is safe here by construction: renderMarkdown returns
                   DOMPurify-sanitized HTML and nothing else is concatenated
                   in. Plain text renders as exactly one <p> wrap. -->
              <div
                class="msg-flow__text"
                :class="{ 'msg-flow__text--clamped': isLong(part.text) && !isExpanded(i, j) }"
                v-html="renderMarkdown(part.text)"
              ></div>
              <div v-if="isLong(part.text) && !isExpanded(i, j)" class="msg-flow__disclose">
                <button type="button" class="msg-flow__disclose-btn" @click="togglePart(i, j)">
                  {{ t('requestMessages.expandFull') }}
                </button>
                <span class="msg-flow__disclose-meta">{{ t('requestMessages.partCharsNote', { n: part.text.length.toLocaleString() }) }}</span>
              </div>
            </template>
            <div v-else-if="part.type === 'image'" class="msg-flow__image">
              <ImageIcon :size="14" />
              <span>{{ t('agentSessions.partImage') }}</span>
            </div>
            <span v-else class="msg-flow__tool">{{ part.label }}</span>
          </template>
        </div>
      </div>
    </template>

    <div v-else-if="body.kind === 'placeholder'" class="msg-flow__note">
      {{ t('requestLogs.bodyNotRecorded') }}
    </div>

    <NCollapse v-else class="msg-flow__fallback">
      <NCollapseItem :title="t('agentSessions.flowFallbackTitle')" name="raw">
        <BodyViewer :raw="raw" :raw-hint="t('requestLogs.bodyRawHint')" />
      </NCollapseItem>
    </NCollapse>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NCollapse, NCollapseItem } from 'naive-ui'
import { Image as ImageIcon } from '@lucide/vue'
import type { TranslatedBody } from '../../utils/messageTranslator'
import { renderMarkdown } from '../../utils/markdown'
import BodyViewer from '../request-logs/BodyViewer.vue'

defineProps<{
  body: TranslatedBody
  /** The raw body string, for the fallback JSON view (ignored otherwise). */
  raw: string
}>()

const { t } = useI18n()

// Only the assistant side reads as "the model talking back" — user prompts,
// system prompts, and tool-role messages all belong on the left.
function isAssistant(message: { role: string }): boolean {
  return message.role === 'assistant'
}

// Chip variant per role. Known chat roles get their own tint; anything the
// wire invents lands on the tool tint — still the truth, just colored.
function roleKind(role: string): 'user' | 'assistant' | 'system' | 'tool' {
  if (role === 'user' || role === 'assistant' || role === 'system') return role
  return 'tool'
}

// ---------- Long-part disclosure ----------
// A text part past this many source characters renders clamped to a preview
// with a fade, plus the toggle row. The threshold is measured on the raw
// text (before markdown rendering) so it is deterministic and cheap; 3500
// characters is comfortably past a normal reply and well short of the
// five-figure walls agent tools send.
const COLLAPSE_OVER_CHARS = 3500
const expandedParts = ref(new Set<string>())
const partKey = (i: number, j: number) => `${i}:${j}`
const isLong = (text: string) => text.length > COLLAPSE_OVER_CHARS
const isExpanded = (i: number, j: number) => expandedParts.value.has(partKey(i, j))
function togglePart(i: number, j: number) {
  const key = partKey(i, j)
  const next = new Set(expandedParts.value)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  expandedParts.value = next
}
</script>

<style scoped>
.msg-flow {
  display: flex;
  flex-direction: column;
  gap: var(--space-3, 12px);
}

.msg-flow__truncated {
  margin: 0;
  padding: var(--space-1, 4px) var(--space-2, 8px);
  border-radius: var(--radius-sm, 6px);
  background: var(--color-warning-subtle, #fdf6ec);
  color: var(--color-text-secondary);
  font-size: var(--text-xs, 12px);
}

.msg-flow__row {
  display: flex;
}

.msg-flow__row--left {
  justify-content: flex-start;
}

.msg-flow__row--right {
  justify-content: flex-end;
}

/* Bubbles read as containers: white surface with a hairline border, the
   assistant side alone carries the accent tint — one accent moment per
   page, everything else stays quiet. */
.msg-flow__bubble {
  max-width: 85%;
  /* Flex items default to min-width: auto, so an unbreakable child (a wide
     table cell, a long token the inherited word-break cannot split) would
     push the bubble past max-width and rip the row open — clamp it. */
  min-width: 0;
  padding: var(--space-3, 12px) var(--space-4, 16px);
  border: 1px solid var(--color-border, #e0e0e6);
  border-radius: var(--radius-md, 8px);
  background: var(--color-surface, #fff);
  box-shadow: 0 1px 2px oklch(22% 0.028 250 / 0.04);
  font-size: var(--text-sm, 13px);
  line-height: 1.65;
}

.msg-flow__bubble--assistant {
  background: var(--color-accent-subtle, #eef0fe);
}

/* Role chips replace the old uppercase micro-label: lowercase, tinted per
   role, readable at a glance. The assistant chip sits on the tinted bubble,
   so it inverts to a white pill for contrast. */
.msg-flow__role {
  display: inline-flex;
  align-items: center;
  margin-bottom: 10px;
  padding: 1px 8px;
  border-radius: var(--radius-full, 999px);
  font-size: 11px;
  font-weight: 600;
}

.msg-flow__role--user {
  background: var(--color-cyan-subtle, #e6f5fa);
  color: var(--color-cyan, #2080f0);
}

.msg-flow__role--assistant {
  background: var(--color-surface, #fff);
  color: var(--color-accent, #6467f2);
  border: 1px solid var(--color-border, #e0e0e6);
}

.msg-flow__role--system {
  background: var(--color-bg-soft, #f2f3f5);
  color: var(--color-text-secondary);
}

.msg-flow__role--tool {
  background: var(--color-purple-subtle, #f3efff);
  color: var(--color-purple, #7c3aed);
}

/* Markdown text parts (v-html, sanitized by renderMarkdown). The container
   only sets word-wrapping and a latin line-length cap (CJK fills the bubble
   before the cap bites); structure and spacing come from the rendered
   markdown itself — no pre-wrap, since CommonMark soft breaks are meant to
   collapse like ordinary HTML text. */
.msg-flow__text {
  margin: 0;
  word-break: break-word;
  min-width: 0;
  max-width: 76ch;
}

/* The clamped preview of a long part: fixed height with a fade instead of
   an abrupt cut, so the disclosure row below reads as "there is more".
   The height is a whole multiple of the 21.45px body line and the fade
   band covers the last ~3 lines, so the cut lands on a mostly-faded line
   rather than a half-rendered one. */
.msg-flow__text--clamped {
  max-height: 322px;
  overflow: hidden;
  -webkit-mask-image: linear-gradient(to bottom, #000 80%, transparent 97%);
  mask-image: linear-gradient(to bottom, #000 80%, transparent 97%);
}

/* Disclosure row: a quiet text button carrying the action, a muted size
   note for the bytes behind it. The top-placed (collapse) variant gets a
   touch more bottom margin to separate it from the wall that follows. */
.msg-flow__disclose {
  display: flex;
  align-items: baseline;
  gap: var(--space-2, 8px);
  margin-top: var(--space-2, 8px);
}

.msg-flow__disclose + .msg-flow__text {
  margin-top: var(--space-2, 8px);
}

.msg-flow__disclose-btn {
  appearance: none;
  border: none;
  background: none;
  padding: 0;
  color: var(--color-accent, #6467f2);
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
}

.msg-flow__disclose-btn:hover {
  text-decoration: underline;
}

.msg-flow__disclose-btn:focus-visible {
  outline: 2px solid var(--color-accent, #6467f2);
  outline-offset: 2px;
  border-radius: 2px;
}

.msg-flow__disclose-meta {
  color: var(--color-text-muted);
  font-size: 11px;
  font-variant-numeric: tabular-nums;
}

.msg-flow__text :deep(p) {
  margin: 0 0 var(--space-2, 8px);
}

.msg-flow__text :deep(p:last-child) {
  margin-bottom: 0;
}

.msg-flow__text :deep(h1),
.msg-flow__text :deep(h2),
.msg-flow__text :deep(h3),
.msg-flow__text :deep(h4),
.msg-flow__text :deep(h5),
.msg-flow__text :deep(h6) {
  margin: var(--space-2, 8px) 0 var(--space-1, 4px);
  font-size: var(--text-sm, 13px);
  line-height: 1.4;
}

.msg-flow__text :deep(h1:first-child),
.msg-flow__text :deep(h2:first-child),
.msg-flow__text :deep(h3:first-child),
.msg-flow__text :deep(h4:first-child),
.msg-flow__text :deep(h5:first-child),
.msg-flow__text :deep(h6:first-child) {
  margin-top: 0;
}

.msg-flow__text :deep(ul),
.msg-flow__text :deep(ol) {
  margin: 0 0 var(--space-2, 8px);
  padding-left: 18px;
}

.msg-flow__text :deep(ul:last-child),
.msg-flow__text :deep(ol:last-child) {
  margin-bottom: 0;
}

.msg-flow__text :deep(blockquote) {
  margin: 0 0 var(--space-2, 8px);
  padding: 2px 0 2px 12px;
  border-left: 3px solid var(--color-border, #e0e0e6);
  color: var(--color-text-secondary);
}

/* Fenced code blocks: a recessed, bordered panel distinct from the white
   bubble, horizontally scrollable so long lines never stretch the bubble
   past its max-width. Inline code gets its own subtle chip — except inside
   fenced blocks, where the panel already carries the treatment. */
.msg-flow__text :deep(pre) {
  margin: 0 0 var(--space-2, 8px);
  padding: var(--space-2, 8px) var(--space-3, 12px);
  border: 1px solid var(--color-border-subtle, #eee);
  border-radius: var(--radius-sm, 6px);
  background: var(--color-bg-soft, #f2f3f5);
  overflow-x: auto;
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
  line-height: 1.55;
}

.msg-flow__text :deep(pre:last-child) {
  margin-bottom: 0;
}

.msg-flow__text :deep(code) {
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
  background: var(--color-bg-soft, #f2f3f5);
  border: 1px solid var(--color-border-subtle, #eee);
  border-radius: 4px;
  padding: 0 4px;
}

.msg-flow__text :deep(pre code) {
  background: none;
  border: none;
  padding: 0;
}

.msg-flow__text :deep(table) {
  margin: 0 0 var(--space-2, 8px);
  border-collapse: collapse;
  font-size: var(--text-xs, 12px);
}

.msg-flow__text :deep(table:last-child) {
  margin-bottom: 0;
}

.msg-flow__text :deep(th),
.msg-flow__text :deep(td) {
  padding: var(--space-1, 4px) var(--space-2, 8px);
  border: 1px solid var(--color-border, #e0e0e6);
  text-align: left;
}

.msg-flow__text :deep(th) {
  background: var(--color-bg-soft, #f2f3f5);
  font-weight: 600;
}

/* Minimal highlight.js token palette for the bubbles. Scoped to the bubble
   text on purpose: importing a full hljs theme would also restyle NCode's
   code blocks elsewhere in the app. */
.msg-flow__text :deep(.hljs-keyword),
.msg-flow__text :deep(.hljs-selector-tag),
.msg-flow__text :deep(.hljs-built_in),
.msg-flow__text :deep(.hljs-type) {
  color: var(--color-purple, #7c3aed);
}

.msg-flow__text :deep(.hljs-string),
.msg-flow__text :deep(.hljs-attr),
.msg-flow__text :deep(.hljs-template-variable) {
  color: var(--color-green, #18a058);
}

.msg-flow__text :deep(.hljs-comment),
.msg-flow__text :deep(.hljs-quote) {
  color: var(--color-text-muted);
  font-style: italic;
}

.msg-flow__text :deep(.hljs-number),
.msg-flow__text :deep(.hljs-literal) {
  color: var(--color-warning-text, #d48806);
}

.msg-flow__text :deep(.hljs-title),
.msg-flow__text :deep(.hljs-title.function_) {
  color: var(--color-info-text, #2080f0);
}

.msg-flow__image {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1, 4px);
  padding: var(--space-1, 4px) var(--space-2, 8px);
  border: 1px dashed var(--color-border, #e0e0e6);
  border-radius: var(--radius-sm, 6px);
  color: var(--color-text-secondary);
  font-size: var(--text-xs, 12px);
}

.msg-flow__tool {
  display: inline-block;
  padding: 1px var(--space-2, 8px);
  border-radius: var(--radius-full, 999px);
  background: var(--color-purple-subtle, #f3efff);
  color: var(--color-purple, #7c3aed);
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
}

.msg-flow__note {
  padding: var(--space-2, 8px) var(--space-3, 12px);
  border-radius: var(--radius-sm, 6px);
  background: var(--color-bg-soft, #f2f3f5);
  color: var(--color-text-muted);
  font-size: var(--text-xs, 12px);
}
</style>
