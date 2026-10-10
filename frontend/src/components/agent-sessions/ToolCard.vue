<!-- frontend/src/components/agent-sessions/ToolCard.vue
     The compact tool card the conversation model renders for machine
     tooling: an assistant-side CALL (tool name, expandable arguments) or a
     tool-side RESULT (tool name, first-line preview, expandable output).
     Both are attachments of a turn, not participants in the conversation —
     a quiet one-line header that opens into a capped, scrollable payload
     block on demand. Default state is COLLAPSED: the card names the
     exchange, the reader opts into its bytes.

     JSON payloads (a call's assembled arguments) render through the app's
     shared hljs core — the same instance the markdown fenced blocks use;
     arbitrary output text stays plain mono (terminal output is
     column-aligned, wrapping it shreds it). -->
<template>
  <div class="toolcard" :class="kind === 'result' ? 'toolcard--result' : 'toolcard--call'">
    <button type="button" class="toolcard__head" @click="open = !open">
      <span class="toolcard__kind">{{ kind === 'result' ? t('agentSessions.toolResult') : t('agentSessions.toolCall') }}</span>
      <span v-if="name" class="toolcard__name">{{ name }}</span>
      <span v-else-if="label" class="toolcard__name toolcard__name--raw">{{ label }}</span>
      <span v-if="kind === 'result' && preview" class="toolcard__preview">{{ preview }}</span>
      <span class="toolcard__toggle">{{ open ? t('agentSessions.collapse') : t('agentSessions.expand') }}</span>
    </button>
    <!-- v-html is safe here by construction: highlightDetail returns
         hljs output or HTML-escaped text, nothing else is concatenated. -->
    <pre v-if="open && detail" class="toolcard__detail"><code v-if="detailLang === 'json'" class="toolcard__detail-code" v-html="highlighted"></code><template v-else>{{ detail }}</template></pre>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import hljs from '../../utils/hljs'

const props = defineProps<{
  /** Which side of the tool exchange: the assistant's call or the tool's result. */
  kind: 'call' | 'result'
  /** The tool's own name ('Bash'); cards without one fall back to `label`. */
  name?: string
  /** Raw wire label for cards without a resolved name ('tool_use', 'tool_calls', …). */
  label?: string
  /** First-line preview (result cards) shown on the collapsed header. */
  preview?: string
  /** The payload worth reading: call arguments / result output. */
  detail?: string
  /** Highlight language for `detail` — 'json' renders through hljs. */
  detailLang?: 'json'
}>()

const { t } = useI18n()
const open = ref(false)

// Same highlight policy as the bubble renderer's tool payloads: every
// fall-off (unknown language, highlighter hiccup) degrades to the same
// block with the text HTML-escaped — never a crash, never raw markup.
const highlighted = computed(() => {
  const text = props.detail ?? ''
  if (props.detailLang !== 'json' || !hljs.getLanguage('json')) {
    return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  }
  try {
    return hljs.highlight(text, { language: 'json' }).value
  } catch {
    return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  }
})
</script>

<style scoped>
/* One quiet card: hairline border, soft surface, mono name. The result
   variant sits on an even softer ground so answers (calls, inside the
   reply bubble) and their consequences (results, between turns) stay
   visually distinct at a glance. */
.toolcard {
  border: 1px solid var(--color-border-subtle, #eee);
  border-radius: var(--radius-sm, 6px);
  background: var(--color-bg-soft, #f2f3f5);
  min-width: 0;
}

.toolcard--result {
  background: var(--color-surface, #fff);
  border-style: dashed;
}

.toolcard__head {
  display: flex;
  align-items: baseline;
  gap: var(--space-2, 8px);
  width: 100%;
  appearance: none;
  border: none;
  background: none;
  padding: var(--space-1, 4px) var(--space-2, 8px);
  font-size: var(--text-xs, 12px);
  line-height: 1.6;
  text-align: left;
  cursor: pointer;
}

.toolcard__head:focus-visible {
  outline: 2px solid var(--color-accent, #6467f2);
  outline-offset: -2px;
  border-radius: var(--radius-sm, 6px);
}

/* The exchange kind reads as a tiny tinted tag; the tool name in mono is
   the card's actual identity. */
.toolcard__kind {
  flex: none;
  padding: 1px 8px;
  border-radius: var(--radius-full, 999px);
  background: var(--color-purple-subtle, #f3efff);
  color: var(--color-purple, #7c3aed);
  font-size: 11px;
  font-weight: 600;
}

.toolcard--result .toolcard__kind {
  background: var(--color-cyan-subtle, #e6f5fa);
  color: var(--color-cyan, #2080f0);
}

.toolcard__name {
  flex: none;
  max-width: 40%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono, monospace);
  font-weight: 600;
  color: var(--color-text-secondary);
}

.toolcard__name--raw {
  font-weight: 400;
  color: var(--color-text-muted);
}

/* The result's first line: quiet, ellipsized — an invitation, not the
   payload itself. */
.toolcard__preview {
  flex: 1 1 auto;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono, monospace);
  color: var(--color-text-muted);
}

.toolcard__toggle {
  flex: none;
  margin-left: auto;
  color: var(--color-accent, #6467f2);
  font-size: 11px;
  font-weight: 500;
}

.toolcard__head:hover .toolcard__toggle {
  text-decoration: underline;
}

/* The expanded payload: mono, capped, scrollable — the same treatment the
   bubble renderer's tool payloads use, so every payload surface in the
   replay reads the same. */
.toolcard__detail {
  margin: 0;
  padding: var(--space-2, 8px) var(--space-3, 12px);
  border-top: 1px solid var(--color-border-subtle, #eee);
  border-radius: 0 0 var(--radius-sm, 6px) var(--radius-sm, 6px);
  background: var(--color-bg-soft, #f2f3f5);
  max-height: 260px;
  overflow: auto;
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
  line-height: 1.55;
  white-space: pre;
}

.toolcard__detail-code {
  font: inherit;
  background: none;
  border: none;
  padding: 0;
}

/* Minimal highlight.js token palette — identical to the bubble flow's, so
   a card's JSON and a fenced block's JSON paint the same. */
.toolcard__detail :deep(.hljs-keyword),
.toolcard__detail :deep(.hljs-selector-tag),
.toolcard__detail :deep(.hljs-built_in),
.toolcard__detail :deep(.hljs-type) {
  color: var(--color-purple, #7c3aed);
}

.toolcard__detail :deep(.hljs-string),
.toolcard__detail :deep(.hljs-attr),
.toolcard__detail :deep(.hljs-template-variable) {
  color: var(--color-green, #18a058);
}

.toolcard__detail :deep(.hljs-comment),
.toolcard__detail :deep(.hljs-quote) {
  color: var(--color-text-muted);
  font-style: italic;
}

.toolcard__detail :deep(.hljs-number),
.toolcard__detail :deep(.hljs-literal) {
  color: var(--color-warning-text, #d48806);
}

.toolcard__detail :deep(.hljs-title),
.toolcard__detail :deep(.hljs-title.function_) {
  color: var(--color-info-text, #2080f0);
}
</style>
