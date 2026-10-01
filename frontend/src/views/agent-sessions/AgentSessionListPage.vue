<!-- frontend/src/views/agent-sessions/AgentSessionListPage.vue
     Tool-session list. One row per agent_session_id — the aggregate the
     backend's GROUP BY computed (handler.GetAgentSessions ->
     requestlog.RequestLogService.ListAgentSessions): tool attribution,
     first/last seen, request + success counts, total tokens, and the
     known/unknown cost doublet rendered as "sum + 'incl. N of unknown cost'".

     The page-top note states the support scope — Claude Code / Codex /
     OpenCode today, with session IDs arriving by two routes: captured
     live from the request's session header (Claude Code, OpenCode), or
     restored by the startup backfill from stored request data (Codex,
     which carries no session header on the live chain) — and points
     tools with no session identifier at the log-audit page's Client
     Tool filter, using the menu name "Log Audit" as the link.

     Server-side paginated (page / page_size, most recent activity first —
     the backend owns the ordering); the single filter is the shared
     agent_client enum (utils/agentClient.ts mirror). Row click opens the
     session detail. -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('agentSessions.eyebrow')" :title="t('agentSessions.pageTitle')" :description="t('agentSessions.pageDescription')" />

    <!-- Support-scope note. The pointer's linked text is the nav menu name
         (nav.logAudit) so the copy and the sidebar stay in sync; the
         sentence around it lives in this page's namespace. -->
    <NAlert type="info" :bordered="false" class="scope-note">
      {{ t('agentSessions.scopeNote') }}
      {{ t('agentSessions.pointerPre') }}<RouterLink to="/request-logs" class="scope-note__link">{{ t('nav.logAudit') }}</RouterLink>{{ t('agentSessions.pointerPost') }}
    </NAlert>

    <div class="filter-panel">
      <div class="filter-grid">
        <!-- Agent-client filter: which calling tool owns the session
             (attribution = the session's earliest row's agent_client).
             Options are the recognizer's closed client enum — the same set
             the request-log list's Client Tool filter offers. -->
        <FilterSelectField
          :label="t('agentSessions.filterAgentClient')"
          :value="agentClient"
          :options="agentClientOptions"
          :placeholder="t('agentSessions.allFilterAgentClient')"
          width="100%"
          @update:value="onAgentClientChange"
        />
      </div>
    </div>

    <EmptyState v-if="!loading && rows.length === 0" :title="t('agentSessions.listEmpty')" />
    <div v-else class="data-table-wrapper">
      <ResponsiveDataTable
        :columns="columns"
        :data="rows"
        :loading="loading"
        :row-key="(row: AgentSessionRow) => row.agent_session_id"
        :row-props="rowProps"
        :pagination="pagination"
        remote
      >
        <template #empty>
          <EmptyState :title="t('agentSessions.listEmpty')" />
        </template>
      </ResponsiveDataTable>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { NTag, useMessage, type DataTableColumns, type PaginationProps, type SelectOption } from 'naive-ui'
import { listAgentSessions, type AgentSessionListParams, type AgentSessionRow } from '../../api/agentSessions'
import { displayMessage } from '../../api/client'
import { formatShortClock } from '../../utils/format'
import { formatMicros } from '../../utils/money'
import { columnTitle } from '../../utils/columnTitle'
import { AGENT_CLIENTS, agentClientLabelKey } from '../../utils/agentClient'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'

const { t } = useI18n()
const router = useRouter()
const message = useMessage()

// The single filter: the calling-tool enum string or null ("all tools" /
// cleared select). Kept as a standalone ref (not a reactive filter object)
// because the page has exactly one constraint.
const agentClient = ref<string | null>(null)

const rows = ref<AgentSessionRow[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)

// Agent-client options mirror the gateway recognizer's client enum
// (utils/agentClient.ts) — the exact set of tool names the backend can
// attribute a session to. Values are the enum strings themselves
// (exact match on the wire); labels are the localized product names the
// request-log pages already use (agentClientLabelKey is the single source
// of truth for tool display names).
const agentClientOptions = computed<SelectOption[]>(() =>
  AGENT_CLIENTS.map((client) => ({
    label: t(agentClientLabelKey(client)),
    value: client,
  })),
)

onMounted(() => {
  void reload().catch((err) => message.error(displayMessage(err, t)))
})

// Monotonic fetch token: a stale list response can't clobber a newer one if
// the user changes the filter before the first load resolves — the same
// guard pattern the request-log list uses.
let fetchId = 0
async function reload() {
  const currentId = ++fetchId
  loading.value = true
  try {
    // agent_client is set only when the dropdown holds a value, so the
    // untouched/cleared filter leaves the key absent entirely (filter off)
    // — the same convention the request-log list's params use.
    const params: AgentSessionListParams = { page: page.value, page_size: pageSize.value }
    if (agentClient.value) params.agent_client = agentClient.value
    const res = await listAgentSessions(params)
    if (currentId !== fetchId) return
    rows.value = res.list
    total.value = res.total
  } catch (err) {
    if (currentId !== fetchId) return
    throw err
  } finally {
    if (currentId === fetchId) loading.value = false
  }
}

// The select's value is already the enum string the backend matches; null
// = cleared = filter off. Every change restarts from page 1 so the pager
// never points past the filtered result set's last page.
function onAgentClientChange(v: string | null) {
  agentClient.value = v
  page.value = 1
  void reload().catch((err) => message.error(displayMessage(err, t)))
}

const pagination = computed<PaginationProps>(() => ({
  page: page.value,
  pageSize: pageSize.value,
  itemCount: total.value,
  showSizePicker: true,
  pageSizes: [10, 20, 50, 100],
  onChange: (p: number) => {
    page.value = p
    void reload().catch((err) => message.error(displayMessage(err, t)))
  },
  onUpdatePageSize: (ps: number) => {
    pageSize.value = ps
    page.value = 1
    void reload().catch((err) => message.error(displayMessage(err, t)))
  },
}))

function goDetail(sessionId: string) {
  router.push(`/agent-sessions/${encodeURIComponent(sessionId)}`)
}

// The whole row is the click target (ResponsiveDataTable forwards rowProps
// to the mobile card too, so tapping a card opens the session as well).
// The click handler ignores non-left clicks and text selection drags the
// same way a plain anchor would; modifier clicks fall through so
// ctrl/cmd-click keeps its browser default (open in new tab does not apply
// to an SPA route, but at least doesn't navigate the current tab).
function rowProps(row: AgentSessionRow): Record<string, unknown> {
  return {
    style: 'cursor: pointer;',
    onClick: (e: MouseEvent) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      goDetail(row.agent_session_id)
    },
  }
}

// ---------- Render helpers ----------

// Two stacked lines (from / to) — a range reads better vertically than the
// "A → B" form when the two timestamps wrap.
function timeRangeCell(row: AgentSessionRow) {
  const line = (label: string, iso: string) =>
    h('div', { style: 'display:flex; gap:5px; align-items:baseline;' }, [
      h('span', { style: 'color:var(--color-text-muted, #909399); font-size:11px; flex-shrink:0;' }, label),
      h('span', { style: 'font-variant-numeric: tabular-nums; font-size:12px; white-space:nowrap;' }, formatShortClock(iso)),
    ])
  return h('div', { style: 'display:flex; flex-direction:column; gap:1px;' }, [
    line(t('agentSessions.firstSeenLabel'), row.first_seen_at),
    line(t('agentSessions.lastSeenLabel'), row.last_seen_at),
  ])
}

// Request count (bold) with the success sub-line — "requests and how many
// of them succeeded" is the column's whole question, answered without
// leaving the cell.
function requestsCell(row: AgentSessionRow) {
  return h('div', { style: 'line-height:1.5;' }, [
    h('span', { style: 'font-weight:600; font-variant-numeric: tabular-nums;' }, String(row.request_count)),
    h(
      'span',
      { style: 'font-size:11px; color:var(--color-text-muted, #909399); margin-left:6px;' },
      t('agentSessions.successCountLine', { n: row.success_count }),
    ),
  ])
}

// Session-total tokens, in / out stacked — same shape as the request-log
// usage cell's token lines.
function tokensCell(row: AgentSessionRow) {
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

// Cost cell: the known-cost sum (display-layer micros -> major unit, the
// same formatMicros the request-log cost cell uses) plus the unknown-rows
// marker whenever the session has any. The marker is the disclosure half
// of the known/unknown doublet — never fold unknown rows into the total.
function costCell(row: AgentSessionRow) {
  const sum = h('span', { style: 'font-variant-numeric: tabular-nums;' }, formatMicros(row.known_cost_micros))
  if (row.unknown_cost_count <= 0) return sum
  return h('div', { style: 'display:flex; flex-direction:column; align-items:flex-end; gap:2px;' }, [
    sum,
    h(
      NTag,
      { size: 'tiny', round: true, bordered: false, type: 'warning' },
      { default: () => t('agentSessions.costUnknownNote', { n: row.unknown_cost_count }) },
    ),
  ])
}

// Tool display name: the shared enum's localized label, falling back to
// the raw value for a client outside this frontend's enum mirror (a newer
// gateway) — the raw name is still the truth about the session.
function toolCell(row: AgentSessionRow) {
  const key = agentClientLabelKey(row.agent_client)
  return key ? t(key) : row.agent_client || '-'
}

const columns = computed<DataTableColumns<AgentSessionRow>>(() => [
  {
    title: columnTitle(t('agentSessions.col_tool'), t('agentSessions.col_tool_tip')),
    key: 'agent_client',
    width: 130,
    render: (row) => toolCell(row),
  },
  {
    title: columnTitle(t('agentSessions.col_session'), t('agentSessions.col_session_tip')),
    key: 'agent_session_id',
    minWidth: 220,
    ellipsis: { tooltip: true },
    render: (row) =>
      h('span', { style: 'font-family:var(--font-mono, monospace); font-size:12px;', title: row.agent_session_id }, row.agent_session_id),
  },
  {
    title: columnTitle(t('agentSessions.col_timeRange'), t('agentSessions.col_timeRange_tip')),
    key: 'time_range',
    width: 175,
    render: (row) => timeRangeCell(row),
  },
  {
    title: columnTitle(t('agentSessions.col_requests'), t('agentSessions.col_requests_tip')),
    key: 'request_count',
    width: 120,
    render: (row) => requestsCell(row),
  },
  {
    title: columnTitle(t('agentSessions.col_tokens'), t('agentSessions.col_tokens_tip')),
    key: 'tokens',
    width: 130,
    render: (row) => tokensCell(row),
  },
  {
    title: columnTitle(t('agentSessions.col_cost'), t('agentSessions.col_cost_tip')),
    key: 'cost',
    width: 150,
    align: 'right',
    render: (row) => costCell(row),
  },
])
</script>

<style scoped>
/* Filter-bar classes (.filter-panel / .filter-grid) are the canonical
   shared classes in styles/global.less — see RequestLogListPage. */

.scope-note {
  margin-bottom: var(--space-4, 16px);
}

/* The pointer's inline link: quiet, matching NAlert's info text, with the
   accent surfacing only on hover/focus so it reads as copy first. */
.scope-note__link {
  color: inherit;
  text-decoration: underline;
  text-decoration-color: var(--color-text-muted, #909399);
  text-underline-offset: 2px;
}

.scope-note__link:hover,
.scope-note__link:focus {
  color: var(--color-accent, #4f6ef7);
  text-decoration-color: currentColor;
}
</style>
