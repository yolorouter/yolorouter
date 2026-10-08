<!-- frontend/src/views/audit/AuditLogListPage.vue
     Admin audit log (/audit-logs, admin-only). Server-side paged with the
     filter set matching the backend handler (audit_log_handler.go):
     admin_user_id / action / resource_type / resource_id / status /
     start / end — each optional, composed as an intersection server-side.

     The trail is immutable: there is no edit, no delete, no detail route —
     the row expands for what would crowd the main columns (the redacted
     detail JSON rendered as per-field diff lines, the reconciliation
     request_id, the user agent). Export CSV streams the current filter via
     the same params. Rows are append-only history (admin_username is a
     capture-time snapshot), so nothing on this page can ever write back. -->
<template>
  <div class="common-page">
    <PageHeader
      :eyebrow="t('auditLogs.eyebrow')"
      :title="t('auditLogs.pageTitle')"
      :description="t('auditLogs.pageDescription')"
    >
      <template #actions>
        <NButton :loading="exporting" :disabled="exporting || loading" @click="onExport">
          <template #icon><Download :size="16" /></template>
          {{ t('auditLogs.exportCsv') }}
        </NButton>
      </template>
    </PageHeader>

    <!-- Filter row. NDatePicker is not in main.ts's create() list, so it is
         imported explicitly below — same note as the request-logs page. -->
    <div class="filter-panel">
      <div class="filter-grid">
        <FilterSelectField
          :label="t('auditLogs.filterAdmin')"
          :value="filter.admin_user_id"
          :options="userOptions"
          :placeholder="t('common.allAccounts')"
          filterable
          width="100%"
          @update:value="onAdminChange"
        />
        <div class="filter-item filter-item--grow">
          <NInput
            v-model:value="filter.action"
            :placeholder="t('auditLogs.filterAction')"
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
            v-model:value="filter.resource_type"
            :placeholder="t('auditLogs.filterResourceType')"
            clearable
            size="small"
            @keyup.enter="onSearch"
            @update:value="onFilterChange"
          />
        </div>
        <div class="filter-item filter-item--search">
          <NInput
            v-model:value="filter.resource_id"
            :placeholder="t('auditLogs.filterResourceId')"
            clearable
            size="small"
            @keyup.enter="onSearch"
            @update:value="onFilterChange"
          />
        </div>
        <FilterSelectField
          :label="t('auditLogs.filterStatus')"
          :value="filter.status"
          :options="statusOptions"
          :placeholder="t('auditLogs.allFilterStatus')"
          width="100%"
          @update:value="onStatusChange"
        />
        <div class="filter-item filter-item--range">
          <!-- Desktop: one datetimerange picker; mobile: two standalone
               datetime pickers, same split-shape rationale as the
               request-logs page. -->
          <NDatePicker
            v-if="!isMobile"
            :value="timeRange"
            type="datetimerange"
            clearable
            size="small"
            :shortcuts="rangeShortcuts"
            :placeholder="t('auditLogs.filterTimeRange')"
            @update:value="onRangeChange"
          />
          <div v-else class="filter-range-split">
            <NDatePicker
              v-model:value="startTime"
              type="datetime"
              clearable
              size="small"
              :placeholder="t('auditLogs.filterStartTime')"
              :is-date-disabled="disableAfterEnd"
              @update:value="onSearch"
            />
            <NDatePicker
              v-model:value="endTime"
              type="datetime"
              clearable
              size="small"
              :placeholder="t('auditLogs.filterEndTime')"
              :is-date-disabled="disableBeforeStart"
              @update:value="onSearch"
            />
          </div>
        </div>
        <div class="filter-actions">
          <NButton size="small" type="primary" @click="onSearch">{{ t('auditLogs.search') }}</NButton>
          <NButton size="small" quaternary @click="onReset">{{ t('auditLogs.reset') }}</NButton>
        </div>
      </div>
    </div>

    <EmptyState v-if="!loading && rows.length === 0" :icon="FileSearch" :title="t('auditLogs.listEmpty')" />
    <div v-else class="data-table-wrapper">
      <ResponsiveDataTable
        :columns="columns"
        :data="rows"
        :loading="loading"
        :row-key="(row: AuditLogRow) => row.id"
        :pagination="pagination"
        remote
      >
        <template #empty>
          <EmptyState :icon="FileSearch" :title="t('auditLogs.listEmpty')" />
        </template>
      </ResponsiveDataTable>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NDatePicker, NInput, NTag, useMessage, type DataTableColumns, type PaginationProps, type SelectOption } from 'naive-ui'
import { Download, FileSearch, Search } from '@lucide/vue'
import { exportAuditLogsCSV, listAuditLogs, type AuditLogRow, type AuditStatus } from '../../api/auditLogs'
import { displayMessage } from '../../api/client'
import { auditDetailLines } from '../../utils/auditDetail'
import { columnTitle } from '../../utils/columnTitle'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import { useUserOptions } from '../../composables/useUserOptions'
import { useIsMobile } from '../../composables/useIsMobile'

const { t } = useI18n()
const message = useMessage()

// Filter state — every field the backend accepts. The three string dims are
// exact literals on the wire; time is a [start, end) pair held as two epoch-ms
// refs (the datetimerange picker's tuple is derived, see timeRange).
interface ListFilter {
  admin_user_id: number | null
  action: string
  resource_type: string
  resource_id: string
  status: AuditStatus | null
}
const filter = reactive<ListFilter>({
  admin_user_id: null,
  action: '',
  resource_type: '',
  resource_id: '',
  status: null,
})
const startTime = ref<number | null>(null)
const endTime = ref<number | null>(null)

const isMobile = useIsMobile()

// Adapter for the desktop datetimerange picker (tuple in, two refs out and
// back) — same shape as the request-logs page.
const timeRange = computed<[number, number] | null>(() =>
  startTime.value != null && endTime.value != null ? [startTime.value, endTime.value] : null,
)

function onRangeChange(v: [number, number] | null) {
  startTime.value = v ? v[0] : null
  endTime.value = v ? v[1] : null
  void onSearch()
}

function disableAfterEnd(ts: number): boolean {
  return endTime.value != null && ts > endTime.value
}
function disableBeforeStart(ts: number): boolean {
  return startTime.value != null && ts < startTime.value
}

const rows = ref<AuditLogRow[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const exporting = ref(false)

const { userOptions, loadUserOptions } = useUserOptions()

const statusOptions = computed<SelectOption[]>(() => ([
  { label: t('auditLogs.status_success'), value: 'success' },
  { label: t('auditLogs.status_denied'), value: 'denied' },
  { label: t('auditLogs.status_error'), value: 'error' },
]))

// Preset range shortcuts — same rolling-window semantics as the
// request-logs page ("last 7 days" ends now, not at midnight).
const rangeShortcuts = computed<Record<string, () => [number, number]>>(() => ({
  [t('auditLogs.rangeToday')]: () => {
    const now = Date.now()
    const startOfToday = new Date()
    startOfToday.setHours(0, 0, 0, 0)
    return [startOfToday.getTime(), now]
  },
  [t('auditLogs.rangeYesterday')]: () => {
    const start = new Date()
    start.setDate(start.getDate() - 1)
    start.setHours(0, 0, 0, 0)
    const end = new Date()
    end.setHours(0, 0, 0, 0)
    return [start.getTime(), end.getTime()]
  },
  [t('auditLogs.range7d')]: () => [Date.now() - 7 * 24 * 60 * 60 * 1000, Date.now()],
  [t('auditLogs.range30d')]: () => [Date.now() - 30 * 24 * 60 * 60 * 1000, Date.now()],
}))

let searchTimer: ReturnType<typeof setTimeout> | null = null
onBeforeUnmount(() => {
  if (searchTimer) {
    clearTimeout(searchTimer)
    searchTimer = null
  }
})

// The filter subset both endpoints share — the CSV export walks every
// matching row server-side, so it sends exactly these params and no page
// window.
function buildFilterParams(): Omit<Parameters<typeof listAuditLogs>[0], 'page' | 'page_size'> {
  const params: Omit<Parameters<typeof listAuditLogs>[0], 'page' | 'page_size'> = {}
  if (filter.admin_user_id != null) params.admin_user_id = filter.admin_user_id
  // Exact-match literals: the trimmed value goes on the wire verbatim — the
  // backend applies `=` and treats an absent param as filter-off.
  if (filter.action.trim()) params.action = filter.action.trim()
  if (filter.resource_type.trim()) params.resource_type = filter.resource_type.trim()
  if (filter.resource_id.trim()) params.resource_id = filter.resource_id.trim()
  if (filter.status) params.status = filter.status
  if (startTime.value != null) params.start = new Date(startTime.value).toISOString()
  if (endTime.value != null) params.end = new Date(endTime.value).toISOString()
  return params
}

function buildListParams(): Parameters<typeof listAuditLogs>[0] {
  return { ...buildFilterParams(), page: page.value, page_size: pageSize.value }
}

// Monotonic fetch token: a stale list response can't clobber a newer one —
// same guard shape as the request-logs page (page-local state, no store).
let fetchId = 0
async function reload() {
  const currentId = ++fetchId
  loading.value = true
  try {
    const res = await listAuditLogs(buildListParams())
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

// Debounced search for the free-text inputs; the select and range controls
// call onSearch directly on their @update:value.
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
  filter.admin_user_id = null
  filter.action = ''
  filter.resource_type = ''
  filter.resource_id = ''
  filter.status = null
  startTime.value = null
  endTime.value = null
  page.value = 1
  void reload().catch((err) => message.error(displayMessage(err, t)))
}

async function onExport() {
  exporting.value = true
  try {
    await exportAuditLogsCSV(buildFilterParams())
    message.success(t('auditLogs.exportSuccess'))
  } catch (err) {
    message.error(displayMessage(err, t) || t('auditLogs.exportFailed'))
  } finally {
    exporting.value = false
  }
}

// FilterSelectField is a controlled input (no v-model), so each select's
// handler writes the reactive filter field and then searches.
function onAdminChange(v: number | null) {
  filter.admin_user_id = v
  void onSearch()
}

function onStatusChange(v: AuditStatus | null) {
  filter.status = v
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

onMounted(() => {
  void reload().catch((err) => message.error(displayMessage(err, t)))
  void loadUserOptions()
})

// ---------- Render helpers ----------

function formatTime(iso: string): string {
  // Locale-aware short timestamp for table density (same granularity as
  // the request-logs list — the seconds matter for a change trail).
  return new Date(iso).toLocaleString(undefined, {
    year: '2-digit',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

function statusTagType(status: AuditStatus): 'success' | 'warning' | 'error' {
  if (status === 'success') return 'success'
  if (status === 'denied') return 'warning'
  return 'error'
}

// One label/value pair inside the expanded row. Inline styles on purpose:
// scoped CSS does not apply to h()-rendered elements.
function expandField(label: string, value: string) {
  return h('div', { style: 'display:flex; align-items:baseline; gap:5px; min-width:170px;' }, [
    h('span', { style: 'font-size:12px; color:var(--color-text-muted, #909399); white-space:nowrap; flex-shrink:0;' }, `${label}:`),
    h('span', { style: `font-size:13px; ${value ? '' : 'color:#bbb;'} word-break:break-all;` }, value || '-'),
  ])
}

// The expanded row carries the redacted detail payload (settings diffs as
// per-field "before → after" lines, event context as key: value — parsed by
// utils/auditDetail; unparseable detail degrades to the raw string) plus
// the reconciliation id and the caller's user agent.
function renderAuditExpand(row: AuditLogRow) {
  const children: ReturnType<typeof expandField>[] = [
    expandField(t('auditLogs.col_requestId'), row.request_id),
    expandField(t('auditLogs.col_userAgent'), row.user_agent),
  ]
  if (row.admin_user_id != null) {
    children.unshift(expandField(t('auditLogs.col_adminId'), String(row.admin_user_id)))
  }
  const lines = auditDetailLines(row.detail)
  if (lines) {
    for (const line of lines) {
      children.unshift(expandField(line.field, line.text))
    }
  } else if (row.detail) {
    children.unshift(expandField(t('auditLogs.col_detail'), row.detail))
  }
  if (row.error_brief) {
    children.push(expandField(t('auditLogs.col_errorBrief'), row.error_brief))
  }
  return h('div', { style: 'display:flex; flex-wrap:wrap; gap:8px 24px; padding:10px 16px;' }, children)
}

// The status column lives in both layouts, so its tag render is shared.
function statusCell(row: AuditLogRow) {
  return h(
    NTag,
    { size: 'small', bordered: false, type: statusTagType(row.status) },
    { default: () => t(`auditLogs.status_${row.status}`) },
  )
}

const sharedColumns = computed<DataTableColumns<AuditLogRow>>(() => [
  {
    title: columnTitle(t('auditLogs.col_created'), t('auditLogs.col_created_tip')),
    key: 'created_at',
    width: 150,
    render: (row) => h('span', { style: 'font-variant-numeric: tabular-nums; font-size:12px;' }, formatTime(row.created_at)),
  },
  {
    title: columnTitle(t('auditLogs.col_admin'), t('auditLogs.col_admin_tip')),
    key: 'admin_username',
    width: 120,
    ellipsis: { tooltip: true },
    // '' happens on unauthenticated audit rows (public auth endpoints
    // before any session exists, e.g. a failed console login) — those read
    // as '-' in the admin column, never as a blank cell.
    render: (row) => row.admin_username || '-',
  },
  {
    title: columnTitle(t('auditLogs.col_action'), t('auditLogs.col_action_tip')),
    key: 'action',
    width: 200,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { style: 'font-size:12px;' }, row.action),
  },
  {
    title: columnTitle(t('auditLogs.col_resource'), t('auditLogs.col_resource_tip')),
    key: 'resource_type',
    minWidth: 160,
    render: (row) => {
      // Composite cell: the resource namespace plus the id the audited
      // request's path carried ("" on namespace-less events like logins).
      const children = [
        h('div', { style: 'font-weight:600; font-size:12px;' }, row.resource_type || '-'),
      ]
      if (row.resource_id) {
        children.push(h('div', { style: 'font-size:12px; color:var(--color-text-muted, #909399); margin-top:2px;' }, row.resource_id))
      }
      return h('div', { style: 'line-height:1.5;' }, children)
    },
  },
  {
    title: columnTitle(t('auditLogs.col_status'), t('auditLogs.col_status_tip')),
    key: 'status',
    width: 96,
    align: 'center',
    render: (row) => statusCell(row),
  },
  {
    title: columnTitle(t('auditLogs.col_errorBrief'), t('auditLogs.col_errorBrief_tip')),
    key: 'error_brief',
    minWidth: 140,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { style: `font-size:12px; ${row.error_brief ? '' : 'color:#bbb;'}` }, row.error_brief || '-'),
  },
  {
    title: columnTitle(t('auditLogs.col_sourceIp'), t('auditLogs.col_sourceIp_tip')),
    key: 'source_ip',
    width: 130,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { style: `font-size:12px; ${row.source_ip ? '' : 'color:#bbb;'}` }, row.source_ip || '-'),
  },
])

// Mobile cards treat the first column as the header and cannot expand, so
// the expand column exists only on desktop; on mobile the same fields
// become ordinary labeled card rows.
const mobileDetailColumns = computed<DataTableColumns<AuditLogRow>>(() => [
  { title: columnTitle(t('auditLogs.col_requestId'), t('auditLogs.col_requestId_tip')), key: 'request_id', render: (row) => row.request_id },
  { title: columnTitle(t('auditLogs.col_detail'), t('auditLogs.col_detail_tip')), key: 'detail', render: (row) => row.detail || '-' },
  { title: columnTitle(t('auditLogs.col_userAgent'), t('auditLogs.col_userAgent_tip')), key: 'user_agent', render: (row) => row.user_agent || '-' },
])

const columns = computed<DataTableColumns<AuditLogRow>>(() =>
  isMobile.value
    ? [...sharedColumns.value, ...mobileDetailColumns.value]
    : [{ type: 'expand', expandable: () => true, renderExpand: renderAuditExpand }, ...sharedColumns.value],
)
</script>

<style scoped>
/* Filter-bar styles (.filter-panel / .filter-grid / .filter-item /
   .filter-actions) are the canonical shared classes in styles/global.less —
   same reference shape as the request-logs page. */

/* Mobile-only: the datetimerange picker splits into two stacked datetime
   pickers so each fits the narrow viewport. */
.filter-range-split {
  display: flex;
  gap: var(--space-2);
  width: 100%;
}

.filter-range-split :deep(.n-date-picker) {
  width: 100%;
}
</style>
