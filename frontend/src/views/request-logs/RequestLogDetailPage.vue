<!-- frontend/src/views/request-logs/RequestLogDetailPage.vue
     M6.1 §6.8.4 request-log detail. Renders the metadata sections defined
     in the M6.1 wire schema (service.RequestLogDetail): basic info, model
     info, attempts sequence, usage + cost. PRD §6.8.4 also lists 流式信息 /
     函数调用 / 请求正文 / 响应正文 — those land in M6.2 with the schema
     migration (design doc §9), so this page surfaces a single M6.2 notice
     rather than rendering empty placeholders for fields the backend
     doesn't return yet.

     The attempts array is rendered as NDataTable rather than NTimeline:
     each row carries 7 fields (provider / model / key / outcome / status
     / fail_reason / index), which doesn't fit a timeline node cleanly and
     benefits from column-level tooltips. -->
<template>
  <div class="request-log-detail-page">
    <PageHeader :eyebrow="t('requestLogs.detailEyebrow')" :title="t('requestLogs.detailTitle')" :description="detail?.request_id">
      <template #actions>
        <NButton quaternary size="small" @click="onBack">{{ t('requestLogs.backToList') }}</NButton>
      </template>
    </PageHeader>

    <div v-if="loading" class="loading-state">{{ t('common.loading') }}</div>

    <EmptyState v-else-if="notFound" :title="t('requestLogs.notFound')" >
      <template #action>
        <NButton quaternary size="small" @click="onBack">{{ t('requestLogs.backToList') }}</NButton>
      </template>
    </EmptyState>

    <template v-else-if="detail">
      <!-- Basic info -->
      <section class="section-card">
        <h2 class="section-title">{{ t('requestLogs.sectionBasic') }}</h2>
        <NDescriptions :column="2" label-placement="left" bordered>
          <NDescriptionsItem :label="t('requestLogs.fieldRequestId')">
            <span class="mono-cell">{{ detail.request_id }}</span>
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldCreatedAt')">
            {{ formatTimeFull(detail.created_at) }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.col_owner')">
            {{ detail.owner_label || '—' }}
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
          <NDescriptionsItem v-if="detail.fail_reason" :label="t('requestLogs.fieldFailReason')" :span="2">
            <span class="fail-reason-cell">{{ detail.fail_reason }}</span>
          </NDescriptionsItem>
        </NDescriptions>
      </section>

      <!-- Model info -->
      <section class="section-card">
        <h2 class="section-title">{{ t('requestLogs.sectionModel') }}</h2>
        <NDescriptions :column="2" label-placement="left" bordered>
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
        <p v-if="detail.attempts_detail.length === 0" class="empty-hint">{{ t('requestLogs.attemptsEmpty') }}</p>
        <div v-else class="data-table-wrapper">
          <NDataTable
            :columns="attemptColumns"
            :data="detail.attempts_detail"
            :bordered="false"
            :single-line="false"
            :row-key="(row: AttemptRecord) => `${row.candidate_id}-${row.key_id}`"
          />
        </div>
      </section>

      <!-- Usage + cost -->
      <section class="section-card">
        <h2 class="section-title">{{ t('requestLogs.sectionUsage') }}</h2>
        <NDescriptions :column="2" label-placement="left" bordered>
          <NDescriptionsItem :label="t('requestLogs.fieldInputTokens')">
            {{ detail.input_tokens.toLocaleString() }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldOutputTokens')">
            {{ detail.output_tokens.toLocaleString() }}
          </NDescriptionsItem>
          <NDescriptionsItem :label="t('requestLogs.fieldCost')" :span="2">
            <span v-if="detail.cost_known" class="cost-cell">{{ formatCents(detail.cost_cents) }} {{ t('requestLogs.currencyUnit') }}</span>
            <NTag v-else size="small" :bordered="false" type="default">{{ t('requestLogs.costUnknown') }}</NTag>
          </NDescriptionsItem>
        </NDescriptions>
      </section>

      <!-- M6.2 notice: bodies / stream chunks / tool calls / cache tokens
           are deferred (design doc §9). Single alert rather than four
           empty placeholders so it's clear what's deferred vs what's just
           empty for this particular request. -->
      <NAlert type="info" :bordered="false" :show-icon="true">
        {{ t('requestLogs.bodyNotRecorded') }}
      </NAlert>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NDataTable,
  NDescriptions,
  NDescriptionsItem,
  NTag,
  useMessage,
  type DataTableColumns,
} from 'naive-ui'
import {
  getRequestLogDetail,
  type AttemptRecord,
  type RequestLogDetail,
} from '../../api/requestLogs'
import { APIError, displayMessage } from '../../api/client'
import { formatCents } from '../../utils/money'
import { columnTitle } from '../../utils/columnTitle'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import StatusClassTag from '../../components/request-logs/StatusClassTag.vue'
import AttemptOutcomeTag from '../../components/request-logs/AttemptOutcomeTag.vue'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const message = useMessage()

const detail = ref<RequestLogDetail | null>(null)
const loading = ref(false)
const notFound = ref(false)

// requestId comes from the URL. Computed (not const) so a future in-place
// navigation between detail pages re-runs the loader via watch — kept
// minimal here because the router currently does full remounts on path
// change, but the computed is still cheaper than threading `route.params`
// through the lifecycle manually.
const requestId = computed(() => decodeURIComponent(String(route.params.requestId ?? '')))

onMounted(() => {
  void reload().catch((err) => message.error(displayMessage(err, t)))
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

// The "final" attempt is the last one in the array — gateway/log.go
// appends each try in order, so the last entry is whatever the relay loop
// settled on (success or terminal failure). Used to surface the final
// provider's model name in the model-info section.
const lastAttempt = computed<AttemptRecord | null>(() => {
  const list = detail.value?.attempts_detail ?? []
  return list.length === 0 ? null : list[list.length - 1]
})

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
    // 3rd try" label PRD §6.8.4 asks for.
    title: columnTitle(t('requestLogs.attempt_index'), t('requestLogs.attempt_index_tip')),
    key: 'index',
    width: 80,
    align: 'center',
    render: (_row, index) => h('span', { class: 'mono-cell' }, String(index + 1)),
  },
  {
    title: columnTitle(t('requestLogs.attempt_provider'), t('requestLogs.attempt_provider_tip')),
    key: 'provider_name',
    minWidth: 140,
    render: (row) => row.provider_name || '—',
  },
  {
    title: columnTitle(t('requestLogs.attempt_model'), t('requestLogs.attempt_model_tip')),
    key: 'provider_model_name',
    minWidth: 160,
    render: (row) => row.provider_model_name || '—',
  },
  {
    title: columnTitle(t('requestLogs.attempt_keyLabel'), t('requestLogs.attempt_keyLabel_tip')),
    key: 'key_label',
    minWidth: 140,
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
])
</script>

<style scoped>
.request-log-detail-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

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
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg, 8px);
}

.section-title {
  margin: 0;
  font-size: var(--text-base);
  font-weight: 600;
  color: var(--color-text);
}

.empty-hint {
  color: var(--color-text-muted);
  font-size: var(--text-sm);
  margin: 0;
}

:deep(.mono-cell) {
  font-family: var(--font-mono, monospace);
  font-variant-numeric: tabular-nums;
  font-size: var(--text-xs);
  color: var(--color-text);
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
</style>
