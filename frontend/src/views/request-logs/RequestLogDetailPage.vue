<!-- frontend/src/views/request-logs/RequestLogDetailPage.vue
     Request-log detail. Renders the metadata sections defined
     in the wire schema (service.RequestLogDetail): basic info, model
     info, attempts sequence, usage + cost. The streaming-info /
     function-calls / request-body / response-body fields land later with the schema
     migration, so this page surfaces a single notice
     rather than rendering empty placeholders for fields the backend
     doesn't return yet.

     The attempts array is rendered as NDataTable rather than NTimeline:
     each row carries 7 fields (provider / model / key / outcome / status
     / fail_reason / index), which doesn't fit a timeline node cleanly and
     benefits from column-level tooltips. -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('requestLogs.detailEyebrow')" :title="t('requestLogs.detailTitle')" :description="detail?.request_id">
      <template #actions>
        <NButton quaternary size="small" @click="onBack">{{ t('requestLogs.backToList') }}</NButton>
      </template>
    </PageHeader>

    <div v-if="loading" class="loading-state">{{ t('common.loading') }}</div>

    <EmptyState v-else-if="notFound" type="compact" :title="t('requestLogs.notFound')" >
      <template #action>
        <NButton quaternary size="small" @click="onBack">{{ t('requestLogs.backToList') }}</NButton>
      </template>
    </EmptyState>

    <template v-else-if="detail">
      <!-- Basic info -->
      <section class="section-card table-card">
        <h2 class="section-title">{{ t('requestLogs.sectionBasic') }}</h2>
        <NDescriptions :column="isMobile ? 1 : 2" label-placement="left" bordered>
          <NDescriptionsItem :label="t('requestLogs.fieldRequestId')">
            <div class="mono-cell">{{ detail.request_id }}</div>
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldCreatedAt')">
            {{ formatTimeFull(detail.created_at) }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.col_user')">
            {{ detail.username || '—' }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldApiKey')">
            {{ detail.api_key_id != null ? `#${detail.api_key_id}` : '—' }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.col_stream')">
            <NTag size="small" :bordered="false" :type="detail.is_stream ? 'info' : 'default'">
              {{ detail.is_stream ? t('requestLogs.stream_true') : t('requestLogs.stream_false') }}
            </NTag>
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.col_status')">
            <StatusClassTag :status="detail.status_class" />
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.col_duration')">
            {{ formatDuration(detail.duration_ms) }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldStatusCode')">
            {{ detail.status_code }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldRequestEndpoint')" :span="2">
            <div class="mono-cell">{{ detail.request_path || '—' }}</div>
          </NDescriptionsItem>
          <NDescriptionsItem v-if="detail.source" :label="t('requestLogs.fieldSource')">
            <NTag size="small" round :bordered="false" type="info">{{ t('requestLogs.sourceBadge') }}</NTag>
          </NDescriptionsItem>
          <NDescriptionsItem v-if="detail.parent_request_id" :label="t('requestLogs.fieldParentRequest')">
            <RouterLink class="parent-link mono-cell" :to="`/request-logs/${encodeURIComponent(detail.parent_request_id)}`">
              {{ detail.parent_request_id }}
            </RouterLink>
          </NDescriptionsItem>
           <NDescriptionsItem :label="t('requestLogs.fieldUpstreamEndpoint')" :span="2">
            <div class="mono-cell">{{ detail.upstream_url || '—' }}</div>
          </NDescriptionsItem>
          <NDescriptionsItem v-if="detail.fail_reason" :label="t('requestLogs.fieldFailReason')" >
            <div class="fail-reason-cell">{{ formatFailReason(detail.fail_reason, t) }}</div>
          </NDescriptionsItem>
        </NDescriptions>
      </section>

      <!-- Model info -->
      <section class="section-card table-card">
        <h2 class="section-title">{{ t('requestLogs.sectionModel') }}</h2>
        <NDescriptions :column="isMobile ? 1 : 2" label-placement="left" bordered>
          <NDescriptionsItem :label="t('requestLogs.fieldExternalModel')">
            <span class="model-cell">{{ detail.model_name }}</span>
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldFinalProvider')">
            {{ detail.provider_name || '—' }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldProviderModel')" :span="2">
            <!-- The final provider's model name lives on the LAST attempt
                 (the one that either succeeded or was the terminal failure),
                 not on the row itself — the row only carries the external
                 model name. Fall back to '—' if there were no attempts. -->
            <span v-if="lastAttempt">{{ lastAttempt.provider_model_name || '—' }}</span>
            <span v-else>—</span>
          </NDescriptionsItem>
        </NDescriptions>
      </section>

      <!-- Attempts sequence -->
      <section class="section-card">
        <h2 class="section-title">{{ t('requestLogs.sectionAttempts') }}</h2>
        <EmptyState
          v-if="detail.attempts_detail.length === 0"
          type="compact"
          :title="t('requestLogs.attemptsEmpty')"
        />
        <div v-else class="data-table-wrapper">
          <ResponsiveDataTable
            :columns="attemptColumns"
            :data="detail.attempts_detail"
            :scroll-x="1180"
            :row-key="(row: AttemptRecord) => `${row.candidate_id}-${row.key_id}`"
          >
          </ResponsiveDataTable>
        </div>
      </section>

      <!-- Usage + cost -->
      <section class="section-card table-card">
        <h2 class="section-title">{{ t('requestLogs.sectionUsage') }}</h2>
        <NDescriptions :column="2" label-placement="left" bordered>
          <NDescriptionsItem :label="t('requestLogs.fieldInputTokens')">
            {{ detail.input_tokens.toLocaleString() }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldOutputTokens')">
            {{ detail.output_tokens.toLocaleString() }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldCacheWriteTokens')">
            {{ detail.cache_write_tokens.toLocaleString() }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldCacheReadTokens')">
            {{ detail.cache_read_tokens.toLocaleString() }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldCost')" :span="2">
            <span v-if="detail.cost_known" class="cost-cell">{{ formatMicros(detail.cost_micros) }} {{ t('requestLogs.currencyUnit') }}</span>
            <NTag v-else size="small" :bordered="false" type="default">{{ t('requestLogs.costUnknown') }}</NTag>
          </NDescriptionsItem>
          <!-- Price snapshot: the four unit prices this row was billed with,
               captured at settlement. All four are null together on rows that
               predate the snapshot or could not be priced — shown as an explicit
               "no snapshot" tag, never as fabricated zeros. -->
          <NDescriptionsItem :label="t('requestLogs.fieldSettledPrices')" :span="2">
            <!-- Loose != so a rolling upgrade (older API binary omitting the
                 settled_* fields entirely -> undefined) renders the honest
                 "no snapshot" tag instead of "Input undefined". -->
            <span v-if="detail.settled_input_price != null" class="settled-prices">
              {{ t('requestLogs.settledPriceValues', {
                input: detail.settled_input_price,
                output: detail.settled_output_price,
                write: detail.settled_cache_write_price,
                read: detail.settled_cache_read_price,
              }) }}
            </span>
            <NTag v-else size="small" :bordered="false" type="default">{{ t('requestLogs.settledPricesNone') }}</NTag>
          </NDescriptionsItem>
          <!-- Image settlement snapshot: what a per-image bill priced by.
               Rendered only when present — every non-image row keeps its
               token snapshot above and nothing here. -->
          <NDescriptionsItem v-if="imageSnapshot" :label="t('requestLogs.fieldImagePricing')" :span="2">
            <span class="settled-prices">
              {{ t('requestLogs.imagePricingValues', {
                count: imageSnapshot.actual_n,
                price: imageSnapshot.unit_price,
                source: imageSnapshot.price_source,
                quality: imageSnapshot.request_quality || '—',
                size: imageSnapshot.request_size || '—',
              }) }}
            </span>
          </NDescriptionsItem>
        </NDescriptions>
      </section>

      <!-- Compression outcome: shown when compression was relevant for this
           request — compressors_applied non-empty, compress_skip_reason
           non-empty, OR compressed_request_body non-empty (audit body persisted
           even when pre-relay rejection zeroed compressors_applied). Tokens
           saved / cost saved are ESTIMATES. Skip reason is i18n'd via the same
           SKIP_REASON_KEYS mapper the cost-optimization page uses. The
           compressed body is shown side-by-side with the original in a
           collapsible card (collapsed by default; both use the same inline
           truncation guard). -->
      <section v-if="showCompressSection" class="section-card">
        <h2 class="section-title">{{ t('requestLogs.sectionCompress') }}</h2>
        <NDescriptions :column="2" label-placement="left" bordered>
          <NDescriptionsItem :label="t('requestLogs.fieldCompressTokensSaved')">
            {{ detail.compress_estimated_tokens_saved.toLocaleString() }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldCompressCostSaved')">
            <span v-if="detail.compress_estimated_cost_saved_micros > 0" class="cost-cell">
              {{ formatMicros(detail.compress_estimated_cost_saved_micros) }} {{ t('requestLogs.currencyUnit') }}
              <NTag size="tiny" :bordered="false" type="warning">{{ t('requestLogs.estimatedLabel') }}</NTag>
            </span>
            <span v-else>—</span>
          </NDescriptionsItem>
          <NDescriptionsItem v-if="detail.compressors_applied" :label="t('requestLogs.fieldCompressorsApplied')" :span="2">
            <NTag v-for="c in detail.compressors_applied.split(',')" :key="c" size="small" :bordered="false" type="info" class="compressor-tag">{{ c }}</NTag>
          </NDescriptionsItem>
          <NDescriptionsItem v-if="detail.compress_skip_reason" :label="t('requestLogs.fieldCompressSkipReason')" :span="2">
            {{ formatSkipReason(detail.compress_skip_reason) }}
          </NDescriptionsItem>
          <!-- Pre-relay rejection note: when a request is rejected before
               reaching upstream, compressors_applied is zeroed (savings not
               counted) but the compressed body is still persisted for audit.
               Surface a short note so the empty savings row is not
               misread as "compression did not run". -->
          <NDescriptionsItem
            v-if="!detail.compressors_applied && detail.compressed_request_body"
            :label="t('requestLogs.fieldCompressorsApplied')"
            :span="2"
          >
            <span class="compress-pre-relay-note">{{ t('requestLogs.compressPreRelayNote') }}</span>
          </NDescriptionsItem>
        </NDescriptions>

        <NCollapse v-if="detail.compressed_request_body" class="compress-body-collapse">
          <NCollapseItem :title="t('requestLogs.compressBodyCompare')" name="compare">
            <div class="compress-body-grid">
              <div class="compress-body-col">
                <p class="compress-body-label">{{ t('requestLogs.compressOriginal') }}</p>
                <BodyViewer :raw="detail.request_body || ''" />
              </div>
              <div class="compress-body-col">
                <p class="compress-body-label">{{ t('requestLogs.compressCompressed') }}</p>
                <BodyViewer :raw="detail.compressed_request_body" />
              </div>
            </div>
          </NCollapseItem>
        </NCollapse>
      </section>

      <!-- Bodies: request/response bodies stored verbatim
           server-side (v0.1 does not scrub body content — only request headers
           are masked). Each inline body is capped server-side (maxInlineBodyBytes)
           with a visible marker so a pathological large body can't freeze the
           tab. Empty string means "not captured" (e.g. an early rejection
           before the body was read) and renders as an EmptyState block rather
           than an empty code block. Stream requests carry the sent SSE on disk instead of in
           response_body/upstream_response_body — that card is lazy-loaded via
           body/stream (full content, no mid-stream truncation, only a
           1GiB anti-OOM backstop). -->
      <div class="body-cards">
        <!-- Request headers: the caller's headers as a JSON
             object with sensitive headers already masked server-side. Only
             shown when captured. -->
        <NCard v-if="detail.request_headers" size="small">
          <template #header>
            <div class="body-card-header">
              <span>{{ t('requestLogs.requestHeaders') }}</span>
              <NButton quaternary size="tiny" @click="copyBody(detail.request_headers)">
                <template #icon><Copy :size="14" /></template>
                {{ t('requestLogs.copyBody') }}
              </NButton>
            </div>
          </template>
          <BodyViewer :raw="detail.request_headers" :raw-hint="t('requestLogs.bodyRawHint')" />
        </NCard>

        <!-- The four non-stream bodies differ only by title + bound field,
             so one v-for replaces four
             near-identical copy-pasted NCard blocks. The stream-body card
             below stays separate: its content is a raw SSE transcript (not a
             single JSON value) and it is lazy-loaded with preview/backstop
             truncation hints, so it doesn't fit either shape. -->
        <NCard v-for="section in bodySections" :key="section.key" size="small">
          <template #header>
            <div class="body-card-header">
              <span>{{ section.title }}</span>
              <NButton v-if="section.body" quaternary size="tiny" @click="copyBody(section.body)">
                <template #icon><Copy :size="14" /></template>
                {{ t('requestLogs.copyBody') }}
              </NButton>
            </div>
          </template>
          <EmptyState v-if="!section.body" type="compact" :icon="FileText" :title="t('requestLogs.bodyNotRecorded')" />
          <BodyViewer v-else :raw="section.body" :raw-hint="t('requestLogs.bodyRawHint')" />
        </NCard>

        <NCard v-if="detail.has_stream_body" size="small">
          <template #header>
            <div class="body-card-header">
              <span>{{ t('requestLogs.streamBody') }}</span>
              <NButton v-if="streamBody" quaternary size="tiny" @click="copyBody(streamBody)">
                <template #icon><Copy :size="14" /></template>
                {{ t('requestLogs.copyBody') }}
              </NButton>
            </div>
          </template>
          <NSpin :show="streamLoading">
            <div class="stream-body-content">
              <StreamBodyViewer v-if="streamBody" :body="streamBody" />
              <EmptyState v-else-if="streamLoaded" type="compact" :icon="FileText" :title="t('requestLogs.bodyNotRecorded')" />
            </div>
          </NSpin>
          <p v-if="streamPreviewTruncated" class="stream-truncated-hint">
            {{ t('requestLogs.streamBodyPreviewTruncated') }}
            <a :href="streamRawUrl" target="_blank" rel="noopener noreferrer">{{ t('requestLogs.streamBodyViewFull') }}</a>
          </p>
          <p v-if="detail.stream_body_truncated" class="stream-truncated-hint">
            {{ t('requestLogs.streamBodyTruncated') }}
          </p>
        </NCard>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NCard,
  NCollapse,
  NCollapseItem,
  NDescriptions,
  NDescriptionsItem,
  NSpin,
  NTag,
  useMessage,
  type DataTableColumns,
} from 'naive-ui'
import { FileText, Copy } from '@lucide/vue'
import {
  getRequestLogDetail,
  streamRequestLogBody,
  type AttemptRecord,
  type ImagePricingSnapshot,
  type RequestLogDetail,
} from '../../api/requestLogs'
import { APIError, displayMessage } from '../../api/client'
import { formatMicros } from '../../utils/money'
import { columnTitle } from '../../utils/columnTitle'
import { copyToClipboard } from '../../utils/clipboard'
import { SKIP_REASON_KEYS } from '../../utils/compressSkipReason'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import StatusClassTag from '../../components/request-logs/StatusClassTag.vue'
import AttemptOutcomeTag from '../../components/request-logs/AttemptOutcomeTag.vue'
import BodyViewer from '../../components/request-logs/BodyViewer.vue'
import StreamBodyViewer from '../../components/request-logs/StreamBodyViewer.vue'
import { formatFailReason } from '../../utils/failReason'
import { useIsMobile } from '../../composables/useIsMobile'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const message = useMessage()

const detail = ref<RequestLogDetail | null>(null)

// Parsed per-image settlement snapshot: the raw column is JSON the server
// built at settlement, and a rolling upgrade can serve a row whose value is
// absent — both cases read as "nothing to show".
const imageSnapshot = computed<ImagePricingSnapshot | null>(() => {
  const raw = detail.value?.image_pricing_snapshot
  if (!raw) return null
  try {
    return JSON.parse(raw) as ImagePricingSnapshot
  } catch {
    return null
  }
})
const loading = ref(false)
const notFound = ref(false)
const isMobile = useIsMobile()

// requestId comes from the URL. Navigation between detail pages (e.g. the
// parent-request link) reloads because the layout keys its router-view by
// the full path, remounting this component; the computed just keeps the
// param read in one place.
const requestId = computed(() => decodeURIComponent(String(route.params.requestId ?? '')))

onMounted(() => {
  void reload()
    .then(() => (detail.value?.has_stream_body ? loadStreamBody() : undefined))
    .catch((err) => message.error(displayMessage(err, t)))
})

// 14005 = errcode.RequestLogNotFound (pkg/errcode/errcode.go). Checking
// by code rather than message-text keeps the not-found detection
// locale-independent — the same envelope comes back whether the admin's
// locale is zh-CN or en, and the APIError's localized message would
// otherwise need a regex per locale.
const REQUEST_LOG_NOT_FOUND_CODE = 14005

async function reload() {
  if (!requestId.value) {
    notFound.value = true
    return
  }
  loading.value = true
  notFound.value = false
  try {
    detail.value = await getRequestLogDetail(requestId.value)
  } catch (err) {
    // 404-equivalent maps to a friendly not-found state; anything else
    // bubbles up to the caller's .catch for a toast.
    if (err instanceof APIError && err.code === REQUEST_LOG_NOT_FOUND_CODE) {
      notFound.value = true
      return
    }
    throw err
  } finally {
    loading.value = false
  }
}

function onBack() {
  router.push('/request-logs')
}

async function copyBody(raw: string) {
  // The success toast keeps its body-specific wording; the failure text
  // is generic, so it shares the console-wide key.
  if (await copyToClipboard(raw)) {
    message.success(t('requestLogs.copyBodySuccess'))
  } else {
    message.error(t('common.copyFailed'))
  }
}

// ---------- Body sections ----------

// The stream body is fetched separately from the JSON detail envelope
// (handler.GetRequestLogBodyStream serves raw bytes off disk, not the
// envelope) so it is loaded lazily right after the detail resolves, rather
// than embedded in RequestLogDetail like the other three bodies.
const streamBody = ref('')
const streamLoading = ref(false)
const streamLoaded = ref(false)
// True when the fetched preview is only a prefix of a larger on-disk capture
// (never buffer/render the full up-to-1GiB capture in
// this page's own JS string/DOM) — the "view full file" link below lets the
// browser handle the rest outside this component entirely.
const streamPreviewTruncated = ref(false)
const streamRawUrl = ref('')

async function loadStreamBody() {
  if (streamLoaded.value || !detail.value?.has_stream_body) return
  streamLoading.value = true
  try {
    const preview = await streamRequestLogBody(requestId.value)
    streamBody.value = preview.text
    streamPreviewTruncated.value = preview.truncated
    streamRawUrl.value = preview.rawUrl
  } finally {
    streamLoading.value = false
    streamLoaded.value = true
  }
}

// The four non-stream body cards, driven by the v-for above (was four
// copy-pasted NCard blocks differing only by title + bound field).
const bodySections = computed(() => [
  { key: 'request', title: t('requestLogs.requestBody'), body: detail.value?.request_body ?? '' },
  { key: 'upstreamRequest', title: t('requestLogs.upstreamRequestBody'), body: detail.value?.upstream_request_body ?? '' },
  { key: 'response', title: t('requestLogs.responseBody'), body: detail.value?.response_body ?? '' },
  { key: 'upstreamResponse', title: t('requestLogs.upstreamResponseBody'), body: detail.value?.upstream_response_body ?? '' },
])

// The "final" attempt is the last one in the array — gateway/log.go
// appends each try in order, so the last entry is whatever the relay loop
// settled on (success or terminal failure). Used to surface the final
// provider's model name in the model-info section.
const lastAttempt = computed<AttemptRecord | null>(() => {
  const list = detail.value?.attempts_detail ?? []
  return list.length === 0 ? null : list[list.length - 1]
})

// showCompressSection: compression is relevant when the engine either modified
// the body (compressors_applied != ''), set a skip reason, or persisted a
// compressed body for audit. The audit-body term covers pre-relay rejections:
// when a request is rejected before reaching upstream, compressors_applied is
// zeroed (savings not counted) but compressed_request_body is still persisted,
// so the section must stay visible to keep the audit body inspectable.
const showCompressSection = computed(() => {
  const d = detail.value
  if (!d) return false
  return (
    d.compressors_applied !== '' ||
    d.compress_skip_reason !== '' ||
    d.compressed_request_body !== ''
  )
})

// formatSkipReason: mirrors CostOptimizationPage's formatSkipReason, using the
// shared SKIP_REASON_KEYS map so both views render identical labels.
function formatSkipReason(code: string): string {
  if (code === '') return t('costOptimization.skipReasonOk')
  const key = SKIP_REASON_KEYS[code]
  if (key) return t(key)
  return t('costOptimization.skipReasonUnknown', { code })
}

// ---------- Render helpers ----------

function formatTimeFull(iso: string): string {
  // Long locale-aware format for the detail page; the list page uses the
  // short variant for density.
  return new Date(iso).toLocaleString(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(2)}s`
}

const attemptColumns = computed<DataTableColumns<AttemptRecord>>(() => [
  {
    // 1-indexed attempt sequence — the gateway writes attempts in the
    // order they happened, so index+1 = the human-friendly "1st, 2nd,
    // 3rd try" label.
    title: columnTitle(t('requestLogs.attempt_index'), t('requestLogs.attempt_index_tip')),
    key: 'index',
    minWidth: 70,
    align: 'center',
    render: (_row, index) => h('span', { class: 'mono-cell' }, String(index + 1)),
  },
  {
    title: columnTitle(t('requestLogs.attempt_provider'), t('requestLogs.attempt_provider_tip')),
    key: 'provider_name',
    minWidth: 200,
    render: (row) => row.provider_name || '—',
  },
  {
    title: columnTitle(t('requestLogs.attempt_model'), t('requestLogs.attempt_model_tip')),
    key: 'provider_model_name',
    minWidth: 170,
    render: (row) => row.provider_model_name || '—',
  },
  {
    title: columnTitle(t('requestLogs.attempt_keyLabel'), t('requestLogs.attempt_keyLabel_tip')),
    key: 'key_label',
    minWidth: 130,
    render: (row) => row.key_label || '—',
  },
  {
    title: columnTitle(t('requestLogs.attempt_outcome'), t('requestLogs.attempt_outcome_tip')),
    key: 'outcome',
    width: 130,
    align: 'center',
    render: (row) => h(AttemptOutcomeTag, { outcome: row.outcome }),
  },
  {
    title: columnTitle(t('requestLogs.attempt_statusCode'), t('requestLogs.attempt_statusCode_tip')),
    key: 'status_code',
    width: 90,
    align: 'center',
    render: (row) => h('span', { class: 'mono-cell' }, String(row.status_code)),
  },
  {
    title: columnTitle(t('requestLogs.attempt_failReason'), t('requestLogs.attempt_failReason_tip')),
    key: 'fail_reason',
    minWidth: 200,
    render: (row) => row.fail_reason || '—',
  },
  {
    title: columnTitle(t('requestLogs.attempt_upstreamEndpoint'), t('requestLogs.attempt_upstreamEndpoint_tip')),
    key: 'upstream_url',
    minWidth: 400,
    render: (row) => h('span', { class: 'mono-cell' }, row.upstream_url || '—'),
  },
])
</script>

<style scoped lang="less">
.loading-state {
  color: var(--color-text-secondary);
  padding: var(--space-8);
  text-align: center;
}

.section-card {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  padding: var(--space-4);
  background: var(--color-bg-elevated, var(--color-bg));
  border: 1px solid #efeff5;
  border-radius: var(--radius-lg, 8px);
}

.section-title {
  margin: 0;
  font-size: var(--text-base);
  font-weight: 600;
  color: var(--color-text);
}

.parent-link {
  color: var(--color-accent);
  text-decoration: none;
}
.parent-link:hover {
  text-decoration: underline;
}
:deep(.mono-cell) {
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs);
  color: var(--color-text);
  word-break: break-word;
}

:deep(.model-cell) {
  font-weight: 600;
  color: var(--color-text);
}

:deep(.cost-cell) {
  font-variant-numeric: tabular-nums;
  font-weight: 600;
}

:deep(.fail-reason-cell) {
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs);
  color: var(--color-danger, var(--color-text));
  word-break: break-word;
}

.body-cards {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.body-card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
}

.body-card-header span {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.stream-body-content {
  min-height: 48px;
}

.stream-truncated-hint {
  margin: var(--space-2) 0 0;
  font-size: var(--text-xs);
  color: var(--color-text-muted, var(--color-text-secondary));
}

.compressor-tag {
  margin-right: 4px;
}

.compress-pre-relay-note {
  font-size: var(--text-xs);
  color: var(--color-text-secondary);
}

.compress-body-collapse {
  margin-top: var(--space-2);
}

.compress-body-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-3);
}

.compress-body-col {
  min-width: 0;
}

.compress-body-label {
  margin: 0 0 var(--space-1);
  font-size: var(--text-xs);
  font-weight: 600;
  color: var(--color-text-secondary);
}

@media (max-width: @mobile-breakpoint) {
  .compress-body-grid {
    grid-template-columns: 1fr;
  }
  .section-card {
    padding: 0;
    gap: 0;
  }
  .section-card.table-card {
    border: 0;
    border-radius: 0;
  }
  .section-card .section-title {
    padding: var(--space-4) var(--space-4);
    border-top: 1px solid #efeff5;
    border-left: 1px solid #efeff5;
    border-right: 1px solid #efeff5;
    border-radius: var(--radius-lg, 8px) var(--radius-lg, 8px) 0 0 ;
  }
}
:deep(.section-card .n-descriptions-table-header) {
  width: 280px !important;
}
:deep(.section-card .n-descriptions-table-content) {
  width: 420px !important;
}

</style>
