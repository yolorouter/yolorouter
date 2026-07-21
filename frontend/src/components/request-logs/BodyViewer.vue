<!-- BodyViewer renders a captured request/response body (PRD §6.8.4 request-log
     detail). When the raw string parses as JSON it uses vue-json-pretty for a
     syntax-highlighted, collapsible tree (deep=2 keeps large payloads readable
     without expanding everything; virtual scrolling keeps a huge body — up to
     the 1GiB stream backstop — from freezing the tab). Non-JSON content (e.g. a
     plain-text upstream error) falls back to a <pre> block. Empty content shows
     the caller-provided empty slot / nothing. -->
<template>
  <div class="body-viewer">
    <VueJsonPretty
      v-if="parsed !== undefined"
      :data="parsed"
      :deep="2"
      :show-length="true"
      :show-line-number="false"
      :collapsed-on-click-brackets="true"
      theme="light"
      :virtual="virtual"
      :height="virtual ? 480 : undefined"
    />
    <pre v-else class="body-viewer__raw">{{ raw }}</pre>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import VueJsonPretty from 'vue-json-pretty'
import 'vue-json-pretty/lib/styles.css'

const props = defineProps<{ raw: string }>()

// Mirrors vue-json-pretty's exported JSONDataType (its :data prop type),
// redeclared locally to avoid importing from the library's internal type
// path. Required for type-checking: the :data bind rejects a plain `unknown`.
type JsonData = string | number | boolean | unknown[] | Record<string, unknown> | null

// parsed is the decoded JSON value when raw is valid JSON, or undefined when
// it isn't (the <pre> fallback path). `undefined` — not `null` — signals
// "not JSON", since a body could legitimately be the literal `null`.
const parsed = computed<JsonData | undefined>(() => {
  if (!props.raw) return undefined
  try {
    return JSON.parse(props.raw) as JsonData
  } catch {
    return undefined
  }
})

// Turn on vue-json-pretty's virtual scrolling only for large payloads —
// below the threshold the fixed-height virtual list adds a scrollbar and
// clips short bodies awkwardly, so plain (non-virtual) rendering reads better.
const virtual = computed(() => props.raw.length > 50_000)

// theme is hardcoded 'light': this app is deliberately light-only
// (src/styles/tokens.less pins color-scheme: light; NConfigProvider is never
// given a dark theme), so following prefers-color-scheme here would render
// the JSON tree dark against an otherwise-light page. When the app gains a
// real dark theme, consume that single shared signal instead of matchMedia.
</script>

<style scoped>
.body-viewer {
  max-height: 480px;
  overflow: auto;
  font-size: var(--text-xs);
}

.body-viewer__raw {
  margin: 0;
  white-space: pre-wrap;
  word-break: break-word;
  font-family: var(--font-mono, monospace);
}
</style>
