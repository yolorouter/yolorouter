<!-- frontend/src/views/agent-sessions/AgentSessionDetailPage.vue
     Tool-session detail (handler.GetAgentSessionDetail): the session's
     attribution plus EVERY request it contains, already chronological from
     the backend (repository orders by created_at, id ASC). A deliberately
     slim table — time / model / five-class status / tokens / duration —
     because the per-request depth lives one click away: any row navigates
     to the existing /request-logs/:requestId detail page, reusing its
     full field set (Trace ID, attempts, bodies, ...). -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('agentSessions.detailEyebrow')" :title="t('agentSessions.detailTitle')" :description="t('agentSessions.detailDescription')">
      <template #actions>
        <NButton quaternary size="small" @click="onBack">{{ t('agentSessions.backToList') }}</NButton>
      </template>
    </PageHeader>

    <div v-if="loading" class="loading-state">{{ t('common.loading') }}</div>

    <EmptyState v-else-if="notFound" type="compact" :title="t('agentSessions.notFound')">
      <template #action>
        <NButton quaternary size="small" @click="onBack">{{ t('agentSessions.backToList') }}</NButton>
      </template>
    </EmptyState>

    <template v-else-if="detail">
      <!-- Session identity strip: attribution (the earliest row's
           agent_client), the verbatim session id, and how many requests the
           timeline below holds. -->
      <div class="session-meta">
        <NTag size="small" round :bordered="false" type="info">{{ toolName }}</NTag>
        <span class="session-meta__id">{{ detail.agent_session_id }}</span>
        <span class="session-meta__count">{{ detail.requests.length }} {{ t('agentSessions.detailRequestsUnit') }}</span>
      </div>

      <div class="data-table-wrapper">
        <ResponsiveDataTable
          :columns="columns"
          :data="detail.requests"
          :loading="loading"
          :row-key="(row: RequestLogRow) => row.request_id"
          :row-props="rowProps"
          :pagination="false"
        />
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { NTag, useMessage, type DataTableColumns } from 'naive-ui'
import { getAgentSessionDetail, type AgentSessionDetail } from '../../api/agentSessions'
import type { RequestLogRow } from '../../api/requestLogs'
import { APIError, displayMessage } from '../../api/client'
import { agentClientLabelKey } from '../../utils/agentClient'
import { columnTitle } from '../../utils/columnTitle'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import StatusClassTag from '../../components/request-logs/StatusClassTag.vue'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const message = useMessage()

const detail = ref<AgentSessionDetail | null>(null)
const loading = ref(false)
const notFound = ref(false)

// sessionId comes from the URL, decoded once here. Session ids are opaque
// tool-side identifiers (uuids for Claude Code, arbitrary strings for
// others), so no shape validation — an unknown id renders the not-found
// state below rather than a validation error.
const sessionId = computed(() => decodeURIComponent(String(route.params.sessionId ?? '')))

onMounted(() => {
  void reload().catch((err) => message.error(displayMessage(err, t)))
})

// 14006 = errcode.AgentSessionNotFound (pkg/errcode/errcode.go). Detected
// by code, not message text, so the not-found rendering is
// locale-independent — same convention as the request-log detail's 14005.
const AGENT_SESSION_NOT_FOUND_CODE = 14006

async function reload() {
  if (!sessionId.value) {
    notFound.value = true
    return
  }
  loading.value = true
  notFound.value = false
  try {
    detail.value = await getAgentSessionDetail(sessionId.value)
  } catch (err) {
    if (err instanceof APIError && err.code === AGENT_SESSION_NOT_FOUND_CODE) {
      notFound.value = true
      return
    }
    throw err
  } finally {
    loading.value = false
  }
}

function onBack() {
  router.push('/agent-sessions')
}

// The timeline's whole point is the hop into the existing request detail:
// every row navigates (the backend's ordering is the timeline's ordering).
// Modifier clicks keep their browser default, same as the list page's rows.
function goRequestDetail(requestId: string) {
  router.push(`/request-logs/${encodeURIComponent(requestId)}`)
}

function rowProps(row: RequestLogRow): Record<string, unknown> {
  return {
    style: 'cursor: pointer;',
    onClick: (e: MouseEvent) => {
if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      goRequestDetail(row.request_id)
    },
  }
}

// Tool display name via the shared enum mirror; a client outside the enum
// (newer gateway) falls back to the raw value — still the truth.
const toolName = computed(() => {
  const client = detail.value?.agent_client ?? ''
  const key = agentClientLabelKey(client)
  return key ? t(key) : client || '-'
})

// ---------- Render helpers ----------

// Same short-locale timestamp granularity the request-log table uses.
function formatTime(iso: string): string {
  return new Date(iso).toLocaleString(undefined, {
    year: '2-digit',
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

// In / out stacked, the same shape the request-log usage cell uses — the
// per-request rows here are deliberately a subset of that table.
function tokensCell(row: RequestLogRow) {
  const line = (label: string, value: number) =>
    h('div', { style: 'display:flex; gap:5px;' }, [
      h('span', { style: 'color:var(--color-text-muted, #909399);' }, label),
      h('span', { style: 'font-variant-numeric: tabular-nums;' }, String(value)),
    ])
  return h(
    'div',
    { style: 'display:inline-flex; flex-direction:column; align-items:flex-start; font-size:12px; line-height:1.5;' },
    [
      line(t('requestLogs.tokenRowIn'), row.input_tokens),
      line(t('requestLogs.tokenRowOut'), row.output_tokens),
    ],
  )
}

const columns = computed<DataTableColumns<RequestLogRow>>(() => [
  {
    title: columnTitle(t('agentSessions.col_created'), t('agentSessions.col_created_tip')),
    key: 'created_at',
    width: 150,
    render: (row) => h('span', { style: 'font-variant-numeric: tabular-nums; font-size:12px;' }, formatTime(row.created_at)),
  },
  {
    title: columnTitle(t('agentSessions.col_model'), t('agentSessions.col_model_tip')),
    key: 'model_name',
    minWidth: 180,
    ellipsis: { tooltip: true },
    render: (row) =>
      h('span', { style: 'font-weight:600; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; min-width:0;', title: row.model_name }, row.model_name),
  },
  {
    title: columnTitle(t('agentSessions.col_status'), t('agentSessions.col_status_tip')),
    key: 'status_class',
    width: 96,
    align: 'center',
    render: (row) => h(StatusClassTag, { status: row.status_class }),
  },
  {
    title: columnTitle(t('agentSessions.detail_col_tokens'), t('agentSessions.detail_col_tokens_tip')),
    key: 'tokens',
    width: 130,
    render: (row) => tokensCell(row),
  },
  {
    title: columnTitle(t('agentSessions.col_duration'), t('agentSessions.col_duration_tip')),
    key: 'duration_ms',
    width: 90,
    align: 'right',
    render: (row) => h('span', { style: 'font-variant-numeric: tabular-nums;' }, formatDuration(row.duration_ms)),
  },
])
</script>

<style scoped>
/* Identity strip above the timeline: attribution tag, verbatim session id
   (monospace, wraps rather than clipping — it is the page's subject), and
   the request count. */
.session-meta {
  display: flex;
  align-items: center;
  gap: var(--space-3, 12px);
  flex-wrap: wrap;
  margin-bottom: var(--space-4, 16px);
}

.session-meta__id {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  color: var(--color-text-secondary);
  word-break: break-all;
}

.session-meta__count {
  font-size: 12px;
  color: var(--color-text-muted);
}
</style>
