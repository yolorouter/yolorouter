<!-- frontend/src/views/agent-sessions/AgentSessionDetailPage.vue
     Tool-session detail (handler.GetAgentSessionDetail): the session's
     attribution plus EVERY request it contains, already chronological from
     the backend (repository orders by created_at, id ASC). The table shows
     newest first by default in BOTH layouts (the mobile card path renders
     the passed array as-is — no table sorter there — so the default order
     lives in the data, not the desktop sort state); the time column is
     sortable back to chronological for reading the conversation flow.

     The page reads as the session's map:
       - a session identity strip (attribution, verbatim session id, and
         the request count the table below holds);
       - a read-only request table (time, model, status, tokens, duration)
         over the whole session, so deep links and refreshes land on the
         full picture without the list endpoint. -->
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
           table below holds. -->
      <div class="session-meta">
        <NTag size="small" round :bordered="false" type="info">{{ toolName }}</NTag>
        <span class="session-meta__id">{{ detail.agent_session_id }}</span>
        <span class="session-meta__count">{{ detail.requests.length }} {{ t('agentSessions.detailRequestsUnit') }}</span>
      </div>

      <!-- The whole session's requests in one table (deep-link safe — the
           detail fetch alone carries everything shown). -->
      <div class="data-table-wrapper">
        <ResponsiveDataTable
          :columns="columns"
          :data="tableRows"
          :loading="loading"
          :row-key="(row: RequestLogRow) => row.request_id"
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
import { formatDuration, formatShortClock } from '../../utils/format'
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

// Tool display name via the shared enum mirror; a client outside the enum
// (newer gateway) falls back to the raw value — still the truth.
const toolName = computed(() => {
  const client = detail.value?.agent_client ?? ''
  const key = agentClientLabelKey(client)
  return key ? t(key) : client || '-'
})

// ---------- Render helpers ----------

// The rows the table renders: newest first BY THE DATA ITSELF, not by the
// desktop table's sort state — the mobile card layout renders the passed
// array as-is (no NDataTable sorter on that path), so the default order
// must live here to hold in BOTH layouts. The wire order stays
// chronological (the backend contract), so a local sort by parsed time
// desc; malformed timestamps parse to NaN and count as 0. The desktop time
// column keeps its sorter for flipping back to chronological interactively.
const tableRows = computed<RequestLogRow[]>(() =>
  [...(detail.value?.requests ?? [])].sort((a, b) => {
    const ta = Date.parse(a.created_at)
    const tb = Date.parse(b.created_at)
    return (Number.isNaN(tb) ? 0 : tb) - (Number.isNaN(ta) ? 0 : ta)
  }),
)

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
    // Newest first by default (the log tables' convention); clicking flips
    // to chronological for reading the conversation top-down. Malformed
    // timestamps parse to NaN and count as 0.
    sorter: (a, b) => {
      const ta = Date.parse(a.created_at)
      const tb = Date.parse(b.created_at)
      return (Number.isNaN(ta) ? 0 : ta) - (Number.isNaN(tb) ? 0 : tb)
    },
    defaultSortOrder: 'descend',
    render: (row) => h('span', { style: 'font-variant-numeric: tabular-nums; font-size:12px;' }, formatShortClock(row.created_at)),
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
/* Identity strip above the table: attribution tag, verbatim session id
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
