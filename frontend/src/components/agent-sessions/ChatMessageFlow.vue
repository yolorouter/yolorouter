<!-- frontend/src/components/agent-sessions/ChatMessageFlow.vue
     Renders one translated body (the messageTranslator's TranslatedBody) as
     the conversation view of the request message page:

       - kind 'messages'    chat bubbles: assistant on the right, every other
                            role on the left, each bubble carrying its role
                            label; EVERY text part goes through the markdown
                            pipeline (renderMarkdown: markdown-it + DOMPurify
                            + highlight.js) — model replies and system prompts
                            both arrive as markdown, so no text bubble is
                            exempt; image parts render as a placeholder block
                            (the payload is deliberately not carried into the
                            DOM), and non-text parts (tool_use / tool_calls /
                            ...) show their type label as a tag
       - kind 'placeholder' a muted "not recorded" note — e.g. a stream
                            request whose response body lives on disk
       - kind 'fallback'    a collapsible formatted-JSON view (the shared
                            BodyViewer), for bodies that cannot be read as
                            chat

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
          <span class="msg-flow__role">{{ message.role || '—' }}</span>
          <template v-for="(part, j) in message.parts" :key="j">
            <!-- v-html is safe here by construction: renderMarkdown returns
                 DOMPurify-sanitized HTML and nothing else is concatenated
                 in. Plain text renders as exactly one <p> wrap. -->
            <div v-if="part.type === 'text'" class="msg-flow__text" v-html="renderMarkdown(part.text)"></div>
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
import { useI18n } from 'vue-i18n'
import { NCollapse, NCollapseItem } from 'naive-ui'
import { Image as ImageIcon } from '@lucide/vue'
import type { TranslatedBody } from '../../utils/messageTranslator'
import { renderMarkdown } from '../../utils/markdown'
import BodyViewer from '../request-logs/BodyViewer.vue'

const props = defineProps<{
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
</script>

<style scoped>
.msg-flow {
  display: flex;
  flex-direction: column;
  gap: var(--space-2, 8px);
}

.msg-flow__truncated {
  margin: 0;
  padding: var(--space-1, 4px) var(--space-2, 8px);
  border-radius: var(--radius-md, 6px);
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

.msg-flow__bubble {
  max-width: 85%;
  /* Flex items default to min-width: auto, so an unbreakable child (a wide
     table cell, a long token the inherited word-break cannot split) would
     push the bubble past max-width and rip the row open — clamp it. */
  min-width: 0;
  padding: var(--space-2, 8px) var(--space-3, 12px);
  border-radius: var(--radius-md, 6px);
  font-size: var(--text-sm, 13px);
  line-height: 1.6;
}

.msg-flow__bubble--other {
  background: var(--color-bg-soft, #f2f3f5);
  color: var(--color-text);
}

.msg-flow__bubble--assistant {
  background: var(--color-accent-subtle, #eef0fe);
  color: var(--color-text);
}

.msg-flow__role {
  display: block;
  margin-bottom: 2px;
  font-size: var(--text-xs, 11px);
  color: var(--color-text-muted);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}

/* Markdown text parts (v-html, sanitized by renderMarkdown). The container
   only sets word-wrapping; structure and spacing come from the rendered
   markdown itself — no pre-wrap, since CommonMark soft breaks are meant to
   collapse like ordinary HTML text. */
.msg-flow__text {
  margin: 0;
  word-break: break-word;
  min-width: 0;
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

/* Fenced code blocks: monospace on a soft panel, horizontally scrollable so
   long lines never stretch the bubble past its max-width. */
.msg-flow__text :deep(pre) {
  margin: 0 0 var(--space-2, 8px);
  padding: var(--space-2, 8px) var(--space-3, 12px);
  border-radius: var(--radius-md, 6px);
  background: var(--color-bg-soft, #f2f3f5);
  overflow-x: auto;
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
  line-height: 1.5;
}

.msg-flow__text :deep(pre:last-child) {
  margin-bottom: 0;
}

.msg-flow__text :deep(code) {
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
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
  border-radius: var(--radius-md, 6px);
  color: var(--color-text-secondary);
  font-size: var(--text-xs, 12px);
}

.msg-flow__tool {
  display: inline-block;
  padding: 1px var(--space-2, 8px);
  border-radius: 999px;
  background: var(--color-purple-subtle, #f3efff);
  color: var(--color-purple, #7c3aed);
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
}

.msg-flow__note {
  padding: var(--space-2, 8px) var(--space-3, 12px);
  border-radius: var(--radius-md, 6px);
  background: var(--color-bg-soft, #f2f3f5);
  color: var(--color-text-muted);
  font-size: var(--text-xs, 12px);
}
</style>
