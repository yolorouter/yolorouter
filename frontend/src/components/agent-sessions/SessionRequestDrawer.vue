<!-- frontend/src/components/agent-sessions/SessionRequestDrawer.vue
     The session detail page's in-place request drawer. Clicking a timeline
     row opens this instead of navigating away, keeping the session's whole
     timeline in view while one request is inspected:

       - opens on demand and only then fetches the existing
         /request-logs/:requestId detail (the row already carries the key
         facts, so they render instantly; the bodies arrive with the fetch)
       - renders the captured conversation through the message translator:
         the request body as bubbles, and the response as bubbles — the
         inline response body, or, for a stream request, the SSE capture
         merged back into the full reply (stream_body)
       - keeps a "view full details" hop to the existing request-log detail
         page for the fields this drawer deliberately does not repeat
         (attempts, upstream bodies, ...)

     The caller owns the open state (v-model:show) and passes the clicked
     timeline row. -->
<template>
  <NDrawer v-model:show="show" placement="right" width="min(560px, 92vw)" :mask-closable="true">
    <NDrawerContent :native-scrollbar="false" closable>
      <template #header>
        <div class="drawer-head">
          <span class="drawer-head__model">{{ request?.model_name ?? '—' }}</span>
          <StatusClassTag v-if="request" :status="request.status_class" />
        </div>
      </template>

      <div v-if="request" class="drawer-body">
        <!-- Key facts: everything the timeline row already knows, rendered
             immediately — the on-demand fetch below only adds the bodies. -->
        <div class="drawer-facts">
          <div class="drawer-facts__id" :title="request.request_id">{{ request.request_id }}</div>
          <dl class="drawer-facts__grid">
            <dt>{{ t('agentSessions.col_created') }}</dt>
            <dd>{{ formatShortClock(request.created_at) }}</dd>
            <dt>{{ t('requestLogs.col_provider') }}</dt>
            <dd>{{ request.provider_name || '—' }}</dd>
            <dt>{{ t('requestLogs.col_stream') }}</dt>
            <dd>
              <NTag size="small" :bordered="false" :type="request.is_stream ? 'info' : 'default'">
                {{ request.is_stream ? t('requestLogs.stream_true') : t('requestLogs.stream_false') }}
              </NTag>
            </dd>
            <dt>{{ t('agentSessions.detail_col_tokens') }}</dt>
            <dd>{{ t('requestLogs.tokenRowIn') }} {{ request.input_tokens }} / {{ t('requestLogs.tokenRowOut') }} {{ request.output_tokens }}</dd>
            <dt>{{ t('agentSessions.col_duration') }}</dt>
            <dd>{{ formatDuration(request.duration_ms) }}</dd>
          </dl>
        </div>

        <!-- Conversation: the translator's three body sides. A failed detail
             fetch keeps the row facts above and swaps both sections for one
             inline error block. -->
        <EmptyState
          v-if="loadError"
          type="compact"
          :title="t('agentSessions.drawerLoadFailed')"
          :description="loadError"
        />
        <template v-else>
          <section class="drawer-section">
            <h4 class="drawer-section__title">{{ t('requestLogs.requestBody') }}</h4>
            <NSpin :show="loading">
              <ChatMessageFlow v-if="detail" :body="requestFlow" :raw="detail.request_body" />
            </NSpin>
          </section>

          <section class="drawer-section">
            <h4 class="drawer-section__title">
              {{ t('requestLogs.responseBody') }}
              <span v-if="mergedFromStream" class="drawer-section__note">{{ t('agentSessions.drawerStreamMergedNote') }}</span>
            </h4>
            <NSpin :show="loading">
              <ChatMessageFlow v-if="detail" :body="responseFlow" :raw="responseRaw" />
            </NSpin>
          </section>
        </template>

        <!-- The escape hatch to the full request-detail page. -->
        <div class="drawer-actions">
          <NButton size="small" @click="goFullDetail">
            <template #icon><ExternalLink :size="14" /></template>
            {{ t('agentSessions.drawerViewFull') }}
          </NButton>
        </div>
      </div>
    </NDrawerContent>
  </NDrawer>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { NButton, NDrawer, NDrawerContent, NSpin, NTag } from 'naive-ui'
import { ExternalLink } from '@lucide/vue'
import { getRequestLogDetail, type RequestLogDetail, type RequestLogRow } from '../../api/requestLogs'
import { displayMessage } from '../../api/client'
import { formatDuration, formatShortClock } from '../../utils/format'
import { translateRequestBody, translateStreamBody, translateResponseBody, type TranslatedBody } from '../../utils/messageTranslator'
import StatusClassTag from '../request-logs/StatusClassTag.vue'
import ChatMessageFlow from './ChatMessageFlow.vue'
import EmptyState from '../EmptyState.vue'

const props = defineProps<{
  /** The clicked timeline row; its key facts render before any fetch. */
  request: RequestLogRow | null
}>()

const show = defineModel<boolean>('show', { required: true })

const { t } = useI18n()
const router = useRouter()

const detail = ref<RequestLogDetail | null>(null)
const loading = ref(false)
const loadError = ref('')

// On opening with a row: fetch that row's detail ON DEMAND — the timeline
// itself never fetches bodies. Watching the pair (open state, request id)
// covers both the open transition and a row swap while open; reopening the
// same row refetches (cheap, always fresh); a failed fetch keeps the
// row-fact header useful and shows the error inline.
watch(
  [show, () => props.request?.request_id],
  ([open]) => {
    if (!open || !props.request) return
    void loadDetail(props.request.request_id)
  },
)

// Monotonic fetch token (the repo's staleness guard convention, same as
// the list page's reload): a fetch superseded while in flight — row A slow,
// drawer reopened on row B — must neither write state nor clear the newer
// fetch's spinner. A token rather than a requestId comparison, because it
// also guards the same-row reopen case (two in-flight fetches for one id).
let fetchId = 0

async function loadDetail(requestId: string) {
  const currentId = ++fetchId
  loading.value = true
  loadError.value = ''
  // Drop the previously shown request's bodies right away: until this
  // fetch lands, the drawer must not render row A's conversation under
  // row B's facts — the placeholder + spinner is the honest state.
  detail.value = null
  try {
    const fetched = await getRequestLogDetail(requestId)
    if (currentId !== fetchId) return
    detail.value = fetched
  } catch (err) {
    if (currentId !== fetchId) return
    // The repo-wide convention (api/client.ts displayMessage): an APIError's
    // already-localized message, or the localized network fallback for
    // NetworkError/timeout — never a raw technical string in the UI.
    loadError.value = displayMessage(err, t)
  } finally {
    if (currentId === fetchId) loading.value = false
  }
}

// ---------- Translation (pure functions, applied per render) ----------

// The request side: bubbles from the OpenAI/Anthropic request shape; an
// empty or untranslatable body degrades to the placeholder/fallback signal
// the flow component renders.
const requestFlow = computed<TranslatedBody>(() => translateRequestBody(detail.value?.request_body ?? ''))

// The response side, in priority order:
//   1. the inline response body (non-stream requests);
//   2. the SSE capture (stream requests) merged back into the full reply;
//   3. neither recorded → the placeholder note — NOT an empty assistant
//      bubble: an early-failed or uncaptured request must read as "nothing
//      was recorded", never as "the model chose to say nothing" (which is
//      what translateStreamBody('') would render). stream_body's emptiness
//      alone carries all the distinction needed: '' means no capture OR an
//      unreadable capture file — has_stream_body would only separate those
//      two states, and the request-log detail page already renders both as
//      this same "not recorded" note, so the drawer matches that
//      convention instead of branching on it.
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

function goFullDetail() {
  if (!props.request) return
  show.value = false
  void router.push(`/request-logs/${encodeURIComponent(props.request.request_id)}`)
}
</script>

<style scoped>
.drawer-head {
  display: flex;
  align-items: center;
  gap: var(--space-2, 8px);
  min-width: 0;
}

.drawer-head__model {
  font-size: var(--text-base, 15px);
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.drawer-body {
  display: flex;
  flex-direction: column;
  gap: var(--space-4, 16px);
}

.drawer-facts__id {
  margin-bottom: var(--space-2, 8px);
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
  color: var(--color-text-secondary);
  word-break: break-all;
}

.drawer-facts__grid {
  display: grid;
  grid-template-columns: auto 1fr;
  gap: var(--space-1, 4px) var(--space-3, 12px);
  margin: 0;
  font-size: var(--text-sm, 13px);
}

.drawer-facts__grid dt {
  color: var(--color-text-muted);
}

.drawer-facts__grid dd {
  margin: 0;
  font-variant-numeric: tabular-nums;
}

.drawer-section__title {
  display: flex;
  align-items: baseline;
  gap: var(--space-2, 8px);
  margin: 0 0 var(--space-2, 8px);
  font-size: var(--text-sm, 13px);
  font-weight: 600;
}

.drawer-section__note {
  font-size: var(--text-xs, 11px);
  font-weight: 400;
  color: var(--color-text-muted);
}

.drawer-actions {
  display: flex;
  justify-content: flex-end;
  padding-top: var(--space-2, 8px);
  border-top: 1px solid var(--color-border-subtle, #eee);
}
</style>
