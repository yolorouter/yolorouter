<!-- frontend/src/views/request-logs/RequestLogListPage.vue
     M6.1 §6.8 request-log list. Server-side paged with a filter set
     matching what the backend handler actually accepts
     (request_log_handler.go): request_id / model_name / provider_id /
     status_class / is_stream / start / end. The PRD §6.8.2 list of filters
     also mentions owner_label and api_key prefix, but the M6.1 backend
     exposes the filter as api_key_id (an admin-facing internal id, not a
     user-typable string) — owner/free-text filtering lands with a later
     backend add, not by silently wiring up a UI control that doesn't work.

     Click row → /request-logs/:requestId detail page. Export CSV streams
     the current filter via the same params. -->
<template>
  <div class="request-logs-page">
    <PageHeader :eyebrow="t('requestLogs.eyebrow')" :title="t('requestLogs.pageTitle')" :description="t('requestLogs.pageDescription')">
      <template #actions>
        <NButton :loading="exporting" :disabled="exporting || loading" @click="onExport">
          <template #icon><Download :size="16" /></template>
          {{ t('requestLogs.exportCsv') }}
        </NButton>
      </template>
    </PageHeader>

    <!-- Filter row. NDatePicker / NSelect are not in main.ts's create()
         list, so they're imported explicitly below (frontend-conventions
         坑1 — silently rendering as unknown elements is the worst-case
         failure mode here, not a typecheck error). -->
    <div class="filter-panel">
      <div class="filter-grid">
        <div class="filter-item filter-item--grow">
          <NInput
            v-model:value="filter.request_id"
            :placeholder="t('requestLogs.filterRequestId')"
            clearable
            size="small"
            @keyup.enter="onSearch"
            @update:value="onFilterChange"
          >
            <template #prefix><Search :size="14" /></template>
          </NInput>
        </div>
        <div class="filter-item filter-item--grow">
          <NInput
            v-model:value="filter.model_name"
            :placeholder="t('requestLogs.filterModel')"
            clearable
            size="small"
            @keyup.enter="onSearch"
            @update:value="onFilterChange"
          />
        </div>
        <div class="filter-item">
          <NSelect
            v-model:value="filter.provider_id"
            :options="providerOptions"
            :placeholder="t('requestLogs.filterProvider')"
            clearable
            size="small"
            @update:value="onSearch"
          />
        </div>
        <div class="filter-item">
          <NSelect
            v-model:value="filter.status"
            :options="statusOptions"
            :placeholder="t('requestLogs.filterStatus')"
            clearable
            size="small"
            @update:value="onSearch"
          />
        </div>
        <div class="filter-item">
          <NSelect
            :value="streamSelect"
            :options="streamOptions"
            :placeholder="t('requestLogs.filterStream')"
            clearable
            size="small"
            @update:value="onStreamChange"
          />
        </div>
        <div class="filter-item filter-item--range">
          <NDatePicker
            v-model:value="timeRange"
            type="datetimerange"
            clearable
            size="small"
            :shortcuts="rangeShortcuts"
            :placeholder="t('requestLogs.filterTimeRange')"
            @update:value="onSearch"
          />
        </div>
        <div class="filter-actions">
          <NButton size="small" type="primary" @click="onSearch">{{ t('requestLogs.search') }}</NButton>
          <NButton size="small" quaternary @click="onReset">{{ t('requestLogs.reset') }}</NButton>
        </div>
      </div>
    </div>

    <EmptyState v-if="!loading && rows.length === 0" :title="t('requestLogs.listEmpty')" />
    <div v-else class="data-table-wrapper">
      <NDataTable
        :columns="columns"
        :data="rows"
        :loading="loading"
        :bordered="false"
        :single-line="false"
        :row-key="(row: RequestLogRow) => row.request_id"
        :row-props="rowProps"
        :pagination="pagination"
        remote
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import {
  NButton,
  NDatePicker,
  NInput,
  NSelect,
  NTag,
  useMessage,
  type DataTableColumns,
  type PaginationProps,
  type SelectOption,
} from 'naive-ui'
import { Download, Search } from '@lucide/vue'
import {
  listRequestLogs,
  exportRequestLogsCSV,
  type RequestLogRow,
  type RequestLogListParams,
  type StatusClass,
} from '../../api/requestLogs'
import { listProviders, type Provider } from '../../api/providers'
import { displayMessage } from '../../api/client'
import { formatCents } from '../../utils/money'
import { columnTitle } from '../../utils/columnTitle'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import StatusClassTag from '../../components/request-logs/StatusClassTag.vue'

const { t } = useI18n()
const router = useRouter()
const message = useMessage()

// Filter state — every field the backend actually accepts. `timeRange` is
// the matching pair (start/end) held together as a tuple because
// NDatePicker's datetimerange mode emits them as one value. We split the
// tuple into RFC3339 strings in buildListParams before sending.
interface ListFilter {
  request_id: string
  model_name: string
  provider_id: number | null
  status: StatusClass | null
  is_stream: boolean | null
}
const filter = reactive<ListFilter>({
  request_id: '',
  model_name: '',
  provider_id: null,
  status: null,
  is_stream: null,
})
// Three-state stream filter UI value. The 'all' sentinel maps to
// filter.is_stream = null at the call site (onStreamChange), NOT via a
// direct v-model two-way binding — NSelect can't cleanly carry null as a
// value, so we drive the binding through :value + @update:value instead.
const streamSelect = ref<'all' | 'stream' | 'non-stream'>('all')
const timeRange = ref<[number, number] | null>(null)

const rows = ref<RequestLogRow[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const exporting = ref(false)

// Providers come from the same admin providers endpoint used by the
// providers store; loaded once on mount. We don't go through the Pinia
// store here because the request-logs page is read-only w.r.t. providers
// and doesn't need the store's race-guard / mutation actions — a one-shot
// fetch is simpler and avoids coupling this page to provider CRUD state.
const providers = ref<Provider[]>([])
const providerOptions = computed<SelectOption[]>(() =>
  providers.value.map((p) => ({ label: p.name, value: p.id })),
)

const statusOptions = computed<SelectOption[]>(() => ([
  { label: t('requestLogs.status_success'), value: 'success' },
  { label: t('requestLogs.status_failed'), value: 'failed' },
  { label: t('requestLogs.status_partial'), value: 'partial' },
  { label: t('requestLogs.status_cancelled'), value: 'cancelled' },
  { label: t('requestLogs.status_rejected'), value: 'rejected' },
]))

const streamOptions = computed<SelectOption[]>(() => ([
  { label: t('requestLogs.stream_all'), value: 'all' },
  { label: t('requestLogs.stream_true'), value: 'stream' },
  { label: t('requestLogs.stream_false'), value: 'non-stream' },
]))

// Preset shortcuts for the date-range picker: today / yesterday / last 7
// days / last 30 days. End is set to "now" for the rolling windows so the
// preset matches the admin's mental model ("last 7 days" includes today),
// not "midnight 7 days ago to midnight now".
const rangeShortcuts = computed<Record<string, () => [number, number]>>(() => ({
  [t('requestLogs.rangeToday')]: () => {
    const now = Date.now()
    const startOfToday = new Date()
    startOfToday.setHours(0, 0, 0, 0)
    return [startOfToday.getTime(), now]
  },
  [t('requestLogs.rangeYesterday')]: () => {
    const start = new Date()
    start.setDate(start.getDate() - 1)
    start.setHours(0, 0, 0, 0)
    const end = new Date()
    end.setHours(0, 0, 0, 0)
    return [start.getTime(), end.getTime()]
  },
  [t('requestLogs.range7d')]: () => [Date.now() - 7 * 24 * 60 * 60 * 1000, Date.now()],
  [t('requestLogs.range30d')]: () => [Date.now() - 30 * 24 * 60 * 60 * 1000, Date.now()],
}))

let searchTimer: ReturnType<typeof setTimeout> | null = null
onBeforeUnmount(() => {
  if (searchTimer) {
    clearTimeout(searchTimer)
    searchTimer = null
  }
})

onMounted(() => {
  void reload().catch((err) => message.error(displayMessage(err, t)))
  void loadProviders().catch((err) => message.error(displayMessage(err, t)))
})

async function loadProviders() {
  const { list } = await listProviders()
  providers.value = list
}

function buildListParams(): RequestLogListParams {
  const params: RequestLogListParams = {
    page: page.value,
    page_size: pageSize.value,
  }
  if (filter.request_id.trim()) params.request_id = filter.request_id.trim()
  if (filter.model_name.trim()) params.model_name = filter.model_name.trim()
  if (filter.provider_id != null) params.provider_id = filter.provider_id
  if (filter.status) params.status = filter.status
  if (filter.is_stream != null) params.is_stream = filter.is_stream
  if (timeRange.value) {
    params.start = new Date(timeRange.value[0]).toISOString()
    params.end = new Date(timeRange.value[1]).toISOString()
  }
  return params
}

// Monotonic fetch token: a stale list response can't clobber a newer one
// if the user fires a second search before the first resolves. Same guard
// pattern the API-key/models stores use, kept inline because this page
// doesn't have a Pinia store — the request-log list is page-local state.
let fetchId = 0
async function reload() {
  const currentId = ++fetchId
  loading.value = true
  try {
    const res = await listRequestLogs(buildListParams())
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

// Debounced search for free-text inputs (request_id, model_name). The two
// NSelect filters call onSearch directly on @update:value, so this debounce
// only fires for keystroke-level changes.
function onFilterChange() {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    void onSearch()
  }, 300)
}

async function onSearch() {
  if (searchTimer) {
    clearTimeout(searchTimer)
    searchTimer = null
  }
  page.value = 1
  try {
    await reload()
  } catch (err) {
    message.error(displayMessage(err, t))
  }
}

function onReset() {
  filter.request_id = ''
  filter.model_name = ''
  filter.provider_id = null
  filter.status = null
  filter.is_stream = null
  streamSelect.value = 'all'
  timeRange.value = null
  page.value = 1
  void reload().catch((err) => message.error(displayMessage(err, t)))
}

async function onExport() {
  exporting.value = true
  try {
    await exportRequestLogsCSV(buildListParams())
    message.success(t('requestLogs.exportSuccess'))
  } catch (err) {
    message.error(displayMessage(err, t) || t('requestLogs.exportFailed'))
  } finally {
    exporting.value = false
  }
}

// onStreamChange decodes the three-state UI value into the boolean-or-null
// the backend expects, then fires a search. Wired via :value + @update:value
// rather than v-model so the 'all' → null mapping happens in one place.
function onStreamChange(v: 'all' | 'stream' | 'non-stream' | null) {
  streamSelect.value = v ?? 'all'
  filter.is_stream = v === 'stream' ? true : v === 'non-stream' ? false : null
  void onSearch()
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

function goDetail(requestId: string) {
  router.push(`/request-logs/${encodeURIComponent(requestId)}`)
}

function rowProps(row: RequestLogRow) {
  return {
    style: 'cursor: pointer',
    onClick: () => goDetail(row.request_id),
  }
}

// ---------- Render helpers ----------

function formatTime(iso: string): string {
  // Locale-aware short timestamp for table density; detail page uses a
  // longer format. The toLocaleString options are kept inline rather than
  // extracted to a util because the table + detail page intentionally use
  // different granularities.
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

function streamCell(row: RequestLogRow) {
  return h(
    NTag,
    { size: 'small', bordered: false, type: row.is_stream ? 'info' : 'default' },
    { default: () => (row.is_stream ? t('requestLogs.stream_true') : t('requestLogs.stream_false')) },
  )
}

function tokenCell(row: RequestLogRow) {
  return h('span', { class: 'token-cell' }, `${row.input_tokens} / ${row.output_tokens}`)
}

function costCell(row: RequestLogRow) {
  if (!row.cost_known) {
    return h(NTag, { size: 'small', bordered: false, type: 'default' }, { default: () => t('requestLogs.costUnknown') })
  }
  return h('span', { class: 'cost-cell' }, formatCents(row.cost_cents))
}

function attemptsCell(row: RequestLogRow) {
  // The backend list-row DTO exposes a single `attempts` count — total
  // candidate tries, including both key rotations within a candidate and
  // candidate failovers. PRD §6.8.3 lists "Key 切换" and "failover" as two
  // columns, but the M6.1 wire schema collapses them into one number; the
  // detail page's attempts_detail array shows the full sequence so the
  // breakdown is still recoverable per-request. A zero-count badge helps
  // spot pre-route rejects (no attempt ever fired).
  if (row.attempts === 0) {
    return h(NTag, { size: 'small', bordered: false, type: 'default' }, { default: () => '0' })
  }
  // >1 means a switch happened; tag amber so the admin's eye lands on
  // failover chains. Exactly 1 = clean single-try success, no decoration.
  if (row.attempts > 1) {
    return h(NTag, { size: 'small', bordered: false, type: 'warning' }, { default: () => String(row.attempts) })
  }
  return h('span', { class: 'attempts-cell' }, String(row.attempts))
}

const columns = computed<DataTableColumns<RequestLogRow>>(() => [
  {
    title: columnTitle(t('requestLogs.col_created'), t('requestLogs.col_created_tip')),
    key: 'created_at',
    width: 180,
    render: (row) => h('span', { class: 'mono-cell' }, formatTime(row.created_at)),
  },
  {
    title: columnTitle(t('requestLogs.col_requestId'), t('requestLogs.col_requestId_tip')),
    key: 'request_id',
    minWidth: 200,
    render: (row) => h('span', { class: 'mono-cell request-id-cell' }, row.request_id),
  },
  {
    title: columnTitle(t('requestLogs.col_owner'), t('requestLogs.col_owner_tip')),
    key: 'owner_label',
    minWidth: 120,
    render: (row) => row.owner_label || '—',
  },
  {
    title: columnTitle(t('requestLogs.col_model'), t('requestLogs.col_model_tip')),
    key: 'model_name',
    minWidth: 160,
    render: (row) => h('span', { class: 'model-cell' }, row.model_name),
  },
  {
    title: columnTitle(t('requestLogs.col_provider'), t('requestLogs.col_provider_tip')),
    key: 'provider_name',
    minWidth: 140,
    render: (row) => row.provider_name || '—',
  },
  {
    title: columnTitle(t('requestLogs.col_stream'), t('requestLogs.col_stream_tip')),
    key: 'is_stream',
    width: 110,
    align: 'center',
    render: (row) => streamCell(row),
  },
  {
    title: columnTitle(t('requestLogs.col_status'), t('requestLogs.col_status_tip')),
    key: 'status_class',
    width: 130,
    align: 'center',
    render: (row) => h(StatusClassTag, { status: row.status_class }),
  },
  {
    title: columnTitle(t('requestLogs.col_attempts'), t('requestLogs.col_attempts_tip')),
    key: 'attempts',
    width: 110,
    align: 'center',
    render: (row) => attemptsCell(row),
  },
  {
    title: columnTitle(t('requestLogs.col_tokens'), t('requestLogs.col_tokens_tip')),
    key: 'tokens',
    width: 150,
    align: 'right',
    render: (row) => tokenCell(row),
  },
  {
    title: columnTitle(t('requestLogs.col_cost'), t('requestLogs.col_cost_tip')),
    key: 'cost',
    width: 110,
    align: 'right',
    render: (row) => costCell(row),
  },
  {
    title: columnTitle(t('requestLogs.col_duration'), t('requestLogs.col_duration_tip')),
    key: 'duration_ms',
    width: 100,
    align: 'right',
    render: (row) => h('span', { class: 'mono-cell' }, formatDuration(row.duration_ms)),
  },
  {
    // Actions column — no tooltip (per frontend-conventions.md). The row
    // itself is already clickable end-to-end (rowProps), so this button is
    // an explicit affordance, not the only entry point.
    title: t('common.actions'),
    key: 'actions',
    width: 100,
    align: 'center',
    render: (row) =>
      h(
        'div',
        { onClick: (e: MouseEvent) => e.stopPropagation() },
        [
          h(
            NButton,
            {
              size: 'small',
              text: true,
              type: 'primary',
              onClick: () => goDetail(row.request_id),
            },
            { default: () => t('requestLogs.viewDetail') },
          ),
        ],
      ),
  },
])
</script>

<style scoped>
.request-logs-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

.filter-panel {
  padding: var(--space-4);
  background: var(--color-bg-elevated, var(--color-bg));
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg, 8px);
}

.filter-grid {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  align-items: center;
}

.filter-item {
  width: 180px;
}

.filter-item--grow {
  flex: 1 1 200px;
  min-width: 200px;
}

.filter-item--range {
  width: 360px;
}

.filter-actions {
  display: inline-flex;
  gap: var(--space-2);
  margin-left: auto;
}

:deep(.mono-cell) {
  font-family: var(--font-mono, monospace);
  font-variant-numeric: tabular-nums;
  font-size: var(--text-xs);
  color: var(--color-text);
}

:deep(.request-id-cell) {
  color: var(--color-text-secondary);
}

:deep(.model-cell) {
  font-weight: 600;
  color: var(--color-text);
}

:deep(.token-cell),
:deep(.cost-cell),
:deep(.attempts-cell) {
  font-variant-numeric: tabular-nums;
  font-size: var(--text-xs);
}

:deep(.cost-cell) {
  font-weight: 600;
}
</style>
