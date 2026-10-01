<!-- frontend/src/views/request-messages/RequestMessagePage.vue
     The request message page (/request-messages/:requestId): the full-page
     conversation view of ONE captured request, replacing the old in-place
     session-detail drawer (drawers were banned as content containers: no
     URL to share, no room for long markdown).

     The page is session-independent by design — any request id deep-links
     straight in (session rows, bookmarks, refreshes), so the future log
     audit surface can reuse the same entry without a session context:

       - fetches the existing /request-logs/:requestId detail on mount (the
         row facts are NOT carried over — a deep link has none, the page
         reads everything from the one fetch)
       - key-facts strip: the eight fields the drawer used to show (request
         id, model, status, provider, stream flag, tokens, duration, time)
       - message flow: the same messageTranslator bodies the drawer fed to
         ChatMessageFlow — request side bubbles, then the response side
         (inline body, or the SSE capture merged back for stream requests)
       - "view full details" hops to the existing request-log detail page
         for everything this page deliberately does not repeat (attempts,
         upstream bodies, ...)
       - back navigation follows the browser history so the caller's scroll
         and filters survive; a deep link with no previous entry falls back
         to the session list -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('requestMessages.eyebrow')" :title="t('requestMessages.pageTitle')" :description="t('requestMessages.pageDescription')">
      <template #actions>
        <NButton quaternary size="small" @click="onBack">{{ t('requestMessages.back') }}</NButton>
      </template>
    </PageHeader>

    <div v-if="loading" class="loading-state">{{ t('common.loading') }}</div>

    <EmptyState v-else-if="notFound" type="compact" :title="t('requestMessages.notFound')">
      <template #action>
        <NButton quaternary size="small" @click="onBack">{{ t('requestMessages.back') }}</NButton>
      </template>
    </EmptyState>

    <!-- A transport-level failure keeps the page (and its back button) and
         shows the localized displayMessage copy inline — never the raw
         technical error string. -->
    <EmptyState v-else-if="loadError" type="compact" :title="t('requestMessages.loadFailed')" :description="loadError" />

    <template v-else-if="detail">
      <!-- Key facts: the eight fields the drawer's header + facts grid
           carried, now one bordered panel — request id (monospace, one-click
           copy) over a compact paired grid. -->
      <div class="msg-facts">
        <div class="msg-facts__id-row">
          <span class="msg-facts__id" :title="detail.request_id">{{ detail.request_id }}</span>
          <NButton size="tiny" quaternary @click="copyId">{{ copiedId ? t('common.copied') : t('common.copy') }}</NButton>
        </div>
        <dl class="msg-facts__grid">
          <dt>{{ t('agentSessions.col_model') }}</dt>
          <dd>{{ detail.model_name || '—' }}</dd>
          <dt>{{ t('requestLogs.col_status') }}</dt>
          <dd><StatusClassTag :status="detail.status_class" /></dd>
          <dt>{{ t('requestLogs.col_provider') }}</dt>
          <dd>{{ detail.provider_name || '—' }}</dd>
          <dt>{{ t('requestLogs.col_stream') }}</dt>
          <dd>
            <NTag size="small" :bordered="false" :type="detail.is_stream ? 'info' : 'default'">
              {{ detail.is_stream ? t('requestLogs.stream_true') : t('requestLogs.stream_false') }}
            </NTag>
          </dd>
          <dt>{{ t('agentSessions.detail_col_tokens') }}</dt>
          <dd>{{ t('requestLogs.tokenRowIn') }} {{ detail.input_tokens }} / {{ t('requestLogs.tokenRowOut') }} {{ detail.output_tokens }}</dd>
          <dt>{{ t('agentSessions.col_duration') }}</dt>
          <dd>{{ formatDuration(detail.duration_ms) }}</dd>
          <dt>{{ t('agentSessions.col_created') }}</dt>
          <dd>{{ formatShortClock(detail.created_at) }}</dd>
        </dl>
      </div>

      <section class="msg-section">
        <h2 class="msg-section__title">
          {{ t('requestLogs.requestBody') }}
          <span v-if="flowCount(requestFlow) > 0" class="msg-section__note">{{ t('requestMessages.sectionCount', { n: flowCount(requestFlow) }) }}</span>
        </h2>
        <ChatMessageFlow :body="requestFlow" :raw="detail.request_body" />
      </section>

      <section class="msg-section">
        <h2 class="msg-section__title">
          {{ t('requestLogs.responseBody') }}
          <span v-if="flowCount(responseFlow) > 0" class="msg-section__note">{{ t('requestMessages.sectionCount', { n: flowCount(responseFlow) }) }}</span>
          <span v-if="mergedFromStream" class="msg-section__note">{{ t('requestMessages.streamMergedNote') }}</span>
        </h2>
        <ChatMessageFlow :body="responseFlow" :raw="responseRaw" />
      </section>

      <!-- The escape hatch to the full request-detail page; also the page's
           landing anchor — opening the conversation scrolls here. -->
      <div class="msg-actions">
        <NButton size="small" @click="goFullDetail">
          <template #icon><ExternalLink :size="14" /></template>
          {{ t('requestMessages.viewFull') }}
        </NButton>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NTag, useMessage } from 'naive-ui'
import { ExternalLink } from '@lucide/vue'
import { getRequestLogDetail, type RequestLogDetail } from '../../api/requestLogs'
import { APIError, displayMessage } from '../../api/client'
import { formatDuration, formatShortClock } from '../../utils/format'
import { translateRequestBody, translateStreamBody, translateResponseBody, type TranslatedBody } from '../../utils/messageTranslator'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import StatusClassTag from '../../components/request-logs/StatusClassTag.vue'
import ChatMessageFlow from '../../components/agent-sessions/ChatMessageFlow.vue'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const message = useMessage()

const detail = ref<RequestLogDetail | null>(null)
const loading = ref(false)
const notFound = ref(false)
const loadError = ref('')

// requestId comes from the URL, decoded once here. Request ids are opaque
// server-generated identifiers, so no shape validation — an unknown id
// renders the not-found state below rather than a validation error.
const requestId = computed(() => decodeURIComponent(String(route.params.requestId ?? '')))

onMounted(() => {
  void reload().catch((err) => message.error(displayMessage(err, t)))
})

// A conversation page opens where conversations open: at the latest turn.
// The reply (and the full-details escape hatch) sit at the bottom; landing
// the viewport there saves the operator the scroll on every visit. This
// rides a post-flush watch on the detail rather than a nextTick inside
// reload: the post flush runs after the branch's DOM is committed, and
// scrollIntoView (not window.scrollTo — the app shell scrolls inside
// naive-ui layout containers, the document itself never overflows) walks
// the real scrollable ancestors. The anchor is located by class at call
// time; deep links and manual reloads both land here.
watch(
  detail,
  (d) => {
    if (!d) return
    // The optional call guards test DOMs that define no scrollIntoView
    // (happy-dom builds vary); real browsers always have it.
    document.querySelector('.msg-actions')?.scrollIntoView?.({ block: 'end', inline: 'nearest' })
  },
  { flush: 'post' },
)

// 14005 = errcode.RequestLogNotFound (pkg/errcode/errcode.go). Detected by
// code, not message text, so the not-found rendering is locale-independent —
// same convention as the request-log detail page.
const REQUEST_LOG_NOT_FOUND_CODE = 14005

async function reload() {
  if (!requestId.value) {
    notFound.value = true
    return
  }
  loading.value = true
  notFound.value = false
  loadError.value = ''
  try {
    detail.value = await getRequestLogDetail(requestId.value)
  } catch (err) {
    if (err instanceof APIError && err.code === REQUEST_LOG_NOT_FOUND_CODE) {
      notFound.value = true
      return
    }
    // The repo-wide convention (api/client.ts displayMessage): an APIError's
    // already-localized message, or the localized network fallback for
    // NetworkError/timeout — never a raw technical string in the UI. Rendered
    // inline below; anything beyond reload() still toasts via the caller.
    loadError.value = displayMessage(err, t)
  } finally {
    loading.value = false
  }
}

// ---------- Translation (pure functions, applied per render) ----------

// The request side: bubbles from the OpenAI/Anthropic request shape; an
// empty or untranslatable body degrades to the placeholder/fallback signal
// the flow component renders.
const requestFlow = computed<TranslatedBody>(() => translateRequestBody(detail.value?.request_body ?? ''))

// The response side, in priority order (the drawer's three-way branch,
// carried over unchanged):
//   1. the inline response body (non-stream requests);
//   2. the SSE capture (stream requests) merged back into the full reply;
//   3. neither recorded → the placeholder note — NOT an empty assistant
//      bubble: an early-failed or uncaptured request must read as "nothing
//      was recorded", never as "the model chose to say nothing" (which is
//      what translateStreamBody('') would render). stream_body's emptiness
//      alone carries all the distinction needed: '' means no capture OR an
//      unreadable capture file — has_stream_body would only separate those
//      two states, and the request-log detail page already renders both as
//      this same "not recorded" note, so this page matches that convention
//      instead of branching on it.
// mergedFromStream marks case 2 in the section title.
const mergedFromStream = computed(
  () => !!detail.value && detail.value.response_body.trim() === '' && detail.value.stream_body.trim() !== '',
)
// The fallback view's raw text is whichever body the flow above was built
// from: for a stream request (case 2, including its unmergeable-capture
// fallback) that is the SSE capture — response_body is '' there by
// definition, and feeding it would collapse the collapsible view into a
// bare hint over an empty <pre>.
const responseRaw = computed(() => {
  const d = detail.value
  if (!d) return ''
  return d.response_body.trim() !== '' ? d.response_body : d.stream_body
})
const responseFlow = computed<TranslatedBody>(() => {
  const d = detail.value
  if (!d) return { kind: 'placeholder', messages: [], truncated: false }
  if (d.response_body.trim() !== '') return translateResponseBody(d.response_body)
  if (d.stream_body.trim() === '') return { kind: 'placeholder', messages: [], truncated: false }
  return translateStreamBody(d.stream_body)
})

// ---------- Helpers ----------

// Message count for a section title. Only the 'messages' kind has any;
// placeholder and fallback sections show no count.
function flowCount(body: TranslatedBody): number {
  return body.kind === 'messages' ? body.messages.length : 0
}

// One-click copy of the request id — the field an operator most often
// carries elsewhere (searches, bug reports). The shared common.copy /
// common.copied keys keep the wording consistent with the rest of the app.
const copiedId = ref(false)
async function copyId() {
  if (!detail.value) return
  try {
    await navigator.clipboard.writeText(detail.value.request_id)
    copiedId.value = true
    message.success(t('common.copied'))
    setTimeout(() => {
      copiedId.value = false
    }, 2000)
  } catch {
    message.error(t('common.copyFailed'))
  }
}

// ---------- Navigation ----------

function onBack() {
  // Browser-history back keeps the caller's scroll position and filters —
  // but only when a previous entry exists. vue-router's own history state
  // records it: `back` holds the previous route's full path on every in-app
  // navigation and stays null on a fresh deep link (new tab / first load on
  // this URL). There, back() would be a no-op that strands the user on an
  // un-leaveable page, so fall back to the session list — the sibling
  // surface that owns this page's rows.
  if (router.options.history.state?.back != null) {
    void router.back()
  } else {
    void router.push('/agent-sessions')
  }
}

function goFullDetail() {
  void router.push(`/request-logs/${encodeURIComponent(requestId.value)}`)
}
</script>

<style scoped>
/* Loading note: same muted/centered/padded treatment as the request-log
   detail page this page links to. The class has no global definition —
   scoped styles do not cross components, so this page carries its own
   copy instead of relying on a dead reference. */
.loading-state {
  color: var(--color-text-secondary);
  padding: var(--space-8);
  text-align: center;
}

/* Key facts: one bordered panel — the request id row (monospace, wraps
   rather than clipping — it is the page's subject — plus a tiny copy
   button) over paired label/value cells; two pairs per row on desktop,
   one on narrow viewports. */
.msg-facts {
  margin-bottom: var(--space-5, 20px);
  padding: var(--space-3, 12px) var(--space-4, 16px);
  border: 1px solid var(--color-border-subtle, #eee);
  border-radius: var(--radius-md, 8px);
  background: var(--color-surface, #fff);
}

.msg-facts__id-row {
  display: flex;
  align-items: center;
  gap: var(--space-2, 8px);
  margin-bottom: var(--space-2, 8px);
}

.msg-facts__id {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  color: var(--color-text-secondary);
  word-break: break-all;
}

.msg-facts__grid {
  display: grid;
  grid-template-columns: repeat(2, max-content 1fr);
  gap: 6px var(--space-5, 20px);
  margin: 0;
  font-size: var(--text-sm, 13px);
}

.msg-facts__grid dt {
  color: var(--color-text-muted);
  font-size: 12px;
  white-space: nowrap;
  align-self: baseline;
}

.msg-facts__grid dd {
  margin: 0;
  font-variant-numeric: tabular-nums;
  font-weight: 500;
}

@media (max-width: 768px) {
  .msg-facts__grid {
    grid-template-columns: max-content 1fr;
  }
}

.msg-section {
  margin-bottom: var(--space-5, 20px);
}

.msg-section__title {
  display: flex;
  align-items: baseline;
  gap: var(--space-2, 8px);
  margin: 0 0 var(--space-3, 12px);
  font-size: var(--text-sm, 13px);
  font-weight: 600;
}

.msg-section__note {
  font-size: var(--text-xs, 11px);
  font-weight: 400;
  color: var(--color-text-muted);
}

.msg-actions {
  display: flex;
  justify-content: flex-end;
  padding-top: var(--space-2, 8px);
  border-top: 1px solid var(--color-border-subtle, #eee);
}
</style>
