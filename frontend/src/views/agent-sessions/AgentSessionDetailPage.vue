<!-- frontend/src/views/agent-sessions/AgentSessionDetailPage.vue
     Tool-session detail (handler.GetAgentSessionDetail): the session's
     attribution plus EVERY request it contains, already chronological from
     the backend (repository orders by created_at, id ASC).

     The page reads as one stay-in-place inspection surface:
       - a top summary card recomputed from the requests array itself (same
         semantics as the list SQL), so deep links and refreshes land on a
         full summary without the list endpoint;
       - a timeline table whose every row carries a waterfall bar aligned on
         the session's real time axis (gaps = think time, bar length =
         duration), hovering for the precise start and duration;
       - clicking a row opens the in-place request drawer (key facts, the
         translated conversation bubbles, and a "view full details" hop to
         the existing /request-logs/:requestId detail page) instead of
         navigating away. -->
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

      <!-- Aggregate figures recomputed from the same requests the timeline
           renders (deep-link safe — no dependency on the list endpoint). -->
      <SessionSummaryCard :requests="detail.requests" />

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

      <!-- The in-place inspector: opened by a row click, closed by mask/Esc/
           its own close button. -->
      <SessionRequestDrawer v-model:show="drawerOpen" :request="drawerRequest" />
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
import SessionSummaryCard from '../../components/agent-sessions/SessionSummaryCard.vue'
import SessionWaterfallBar from '../../components/agent-sessions/SessionWaterfallBar.vue'
import SessionRequestDrawer from '../../components/agent-sessions/SessionRequestDrawer.vue'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const message = useMessage()

const detail = ref<AgentSessionDetail | null>(null)
const loading = ref(false)
const notFound = ref(false)

// Drawer state: the clicked row plus the open flag. The drawer fetches the
// row's full detail (bodies) itself, on demand.
const drawerOpen = ref(false)
const drawerRequest = ref<RequestLogRow | null>(null)

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

// Clicking a row opens the in-place drawer — the page itself never
// navigates away; the drawer owns the hop to the request-detail page via
// its "view full details" button. Modifier clicks keep their browser
// default (the row text stays selectable, cmd-click etc. untouched).
function openDrawer(row: RequestLogRow) {
  drawerRequest.value = row
  drawerOpen.value = true
}

function rowProps(row: RequestLogRow): Record<string, unknown> {
  return {
    style: 'cursor: pointer;',
    onClick: (e: MouseEvent) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      openDrawer(row)
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

// ---------- Waterfall axis (shared by every row's bar) ----------

// The session's time axis: from the earliest created_at to the latest
// (created_at + duration). Every bar maps onto THIS span, so relative
// positions are real time. Malformed timestamps parse to NaN and are
// treated as 0 — one bad row must not blank the whole column; span<=0
// (single instant request) collapses to a full-width bar at the left edge
// inside the bar component.
const timeline = computed(() => {
  const rows = detail.value?.requests ?? []
  const starts = rows.map((r) => {
    const ms = Date.parse(r.created_at)
    return Number.isNaN(ms) ? 0 : ms
  })
  const ends = rows.map((r, i) => {
    const dur = Number.isFinite(r.duration_ms) && r.duration_ms > 0 ? r.duration_ms : 0
    return starts[i] + dur
  })
  if (starts.length === 0) return { startMs: 0, spanMs: 0 }
  const startMs = Math.min(...starts)
  const spanMs = Math.max(...ends) - startMs
  return { startMs, spanMs }
})

// ---------- Render helpers ----------

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
  {
    title: columnTitle(t('agentSessions.col_waterfall'), t('agentSessions.col_waterfall_tip')),
    key: 'waterfall',
    minWidth: 200,
    render: (row) =>
      h(SessionWaterfallBar, {
        row,
        startMs: timeline.value.startMs,
        spanMs: timeline.value.spanMs,
      }),
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
