<!-- frontend/src/views/analytics/AnalyticsPage.vue
     Usage report. Combines:
       - Filter bar (time range / api key / model / provider / status)
       - Dimension tabs (account / model / provider / time / caller)
       - Overview metric row (calls / success rate / tokens / cost)
       - Dimension-specific NDataTable (column tooltips via columnTitle)
       - CSV export button

     The page owns the filter + dimension state. Each change triggers a
     reload of both /overview and /report in parallel — they're independent
     given the same filter, so a single error message covers both. -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('analytics.eyebrow')" :title="t('analytics.pageTitle')" :description="t('analytics.pageDescription')">
      <template #actions>
        <NButton :loading="exporting" :disabled="!reportRows.length" @click="onExport">
          <template #icon><Download :size="16" /></template>
          {{ t('analytics.exportCSV') }}
        </NButton>
      </template>
    </PageHeader>

    <!-- Filter bar (inlined). The page owns filter/time-range/preset state, so
         the controls bind straight to it instead of routing through a wrapper
         component's events. -->
    <div class="filter-panel">
      <div class="filter-grid">
        <div class="filter-item w-auto">
          <TimeRangeSelect
            :model-value="timeRange"
            :preset="preset"
            @update:model-value="onTimeRange"
            @update:preset="onPreset"
          />
        </div>

        <FilterSelectField
          v-if="authStore.isAdmin"
          :label="t('analytics.user')"
          :value="filter.user_id ?? null"
          :options="userOptions"
          :placeholder="t('common.allAccounts')"
          filterable
          width="100%"
          @update:value="(v) => update('user_id', v)"
        />

        <FilterSelectField
          :label="t('analytics.apiKey')"
          :value="filter.api_key_id ?? null"
          :options="apiKeyOptions"
          :placeholder="t('analytics.allApiKey')"
          filterable
          width="100%"
          @update:value="(v) => update('api_key_id', v)"
        />

        <FilterSelectField
          :label="t('analytics.model')"
          :value="filter.model_name ?? null"
          :options="modelOptions"
          :placeholder="t('analytics.allModel')"
          filterable
          width="100%"
          @update:value="(v) => update('model_name', v)"
        />

        <FilterSelectField
          v-if="authStore.isAdmin"
          :label="t('analytics.provider')"
          :value="filter.provider_id ?? null"
          :options="providerOptions"
          :placeholder="t('analytics.allProvider')"
          filterable
          width="100%"
          @update:value="(v) => update('provider_id', v)"
        />

        <FilterSelectField
          :label="t('analytics.status')"
          :value="filter.status ?? null"
          :options="statusOptions"
          :placeholder="t('analytics.allStatus')"
          width="100%"
          @update:value="(v) => update('status', (v as string) || null)"
        />
      </div>
    </div>
    <div v-if="dimension === 'time'" class="bucket-bar">
      <span class="bucket-label">{{ t('analytics.bucketLabel') }}</span>
      <NSelect
        :value="bucket"
        :options="bucketOptions"
        size="small"
        style="width: 120px"
        @update:value="(v: AnalyticsBucket) => onBucketChange(v)"
      />
    </div>

    <!-- Overview metric row -->
    <div class="metric-row">
      <div class="metric">
        <div class="metric__label">
          <HelpLabel :tip="t('analytics.callsColumn_tip')">{{ t('analytics.totalCalls') }}</HelpLabel>
        </div>
        <div class="metric__value">{{ formatNumber(overview?.total_calls ?? 0) }}</div>
      </div>
      <div class="metric">
        <div class="metric__label">
          <HelpLabel :tip="t('analytics.successRate_tip')">{{ t('analytics.successRate') }}</HelpLabel>
        </div>
        <div class="metric__value">{{ formatRate(overview?.success_rate ?? 0) }}</div>
        <div class="metric__sub">{{ t('analytics.successRateSub', { success: overview?.success_calls ?? 0, ended: overview?.ended_calls ?? 0 }) }}</div>
      </div>
      <div class="metric">
        <div class="metric__label">
          <HelpLabel :tip="t('analytics.inputTokensColumn_tip')">{{ t('analytics.inputTokens') }}</HelpLabel>
        </div>
        <div class="metric__value">{{ formatNumber(overview?.input_tokens ?? 0) }}</div>
      </div>
      <div class="metric">
        <div class="metric__label">
          <HelpLabel :tip="t('analytics.outputTokensColumn_tip')">{{ t('analytics.outputTokens') }}</HelpLabel>
        </div>
        <div class="metric__value">{{ formatNumber(overview?.output_tokens ?? 0) }}</div>
      </div>
      <div class="metric">
        <div class="metric__label">
          <HelpLabel :tip="t('analytics.costColumn_tip')">{{ t('analytics.totalCost') }}</HelpLabel>
        </div>
        <div class="metric__value">¥{{ formatMicros(overview?.cost_micros ?? 0, 2) }}</div>
        <div v-if="(overview?.unknown_cost_calls ?? 0) > 0" class="metric__sub">
          {{ t('analytics.unknownCostSub', { n: overview?.unknown_cost_calls ?? 0 }) }}
        </div>
      </div>
    </div>

    <!-- Dimension tabs + report table -->
    <div class="section-card  table-card">
      <NTabs :value="dimension" type="line" @update:value="onDimensionChange">
        <NTabPane v-if="authStore.isAdmin" :name="'user'" :tab="t('analytics.dimensionUser')">
          <!-- Stated in the open rather than in the column tooltip: the
               mobile card layout promotes this dimension's column to the
               card header, which renders the value without its title, so a
               tooltip here would be invisible on a phone. -->
          <p v-if="overviewCountsExcludedTraffic" class="unattributed-note">
            {{ t('analytics.unattributedExcluded') }}
          </p>
          <FilterSelectField
            v-if="isMobile"
            v-model:value="mobileSort"
            :label="t('analytics.mobileSortBy')"
            :options="mobileSortOptions"
            :placeholder="t('analytics.mobileSortDefault')"
            size="small"
            width="100%"
          />
          <ResponsiveDataTable
            :columns="userColumns"
            :data="displayedUserRows"
            :row-props="(r: AttributedUserRow) => drillRowProps({ user_id: r.user_id })"
            :loading="loading"
            :scroll-x="1330"
            :row-key="userRowKey"
          >
            <template #empty>
              <EmptyState :icon="BarChart3" :title="t('analytics.noData')" />
            </template>
          </ResponsiveDataTable>
        </NTabPane>
        <NTabPane :name="'model'" :tab="t('analytics.dimensionModel')">
          <!-- Mobile cards have no sortable headers, so metric sorting
               lives in this shared selector on the four entity tabs. -->
          <FilterSelectField
            v-if="isMobile"
            v-model:value="mobileSort"
            :label="t('analytics.mobileSortBy')"
            :options="mobileSortOptions"
            :placeholder="t('analytics.mobileSortDefault')"
            size="small"
            width="100%"
          />
          <ResponsiveDataTable
            :columns="modelColumns"
            :data="displayedModelRows"
            :row-props="(r: ModelReportRow) => drillRowProps(r.model_name ? { model_name: r.model_name } : null)"
            :loading="loading"
            :scroll-x="1330"
            :row-key="(r: ModelReportRow) => r.model_name"
          >
            <template #empty>
              <EmptyState :icon="BarChart3" :title="t('analytics.noData')" />
            </template>
          </ResponsiveDataTable>
        </NTabPane>
        <NTabPane v-if="authStore.isAdmin" :name="'provider'" :tab="t('analytics.dimensionProvider')">
          <!-- Mobile cards have no sortable headers, so metric sorting
               lives in this shared selector on the four entity tabs. -->
          <FilterSelectField
            v-if="isMobile"
            v-model:value="mobileSort"
            :label="t('analytics.mobileSortBy')"
            :options="mobileSortOptions"
            :placeholder="t('analytics.mobileSortDefault')"
            size="small"
            width="100%"
          />
          <ResponsiveDataTable
            :columns="providerColumns"
            :data="displayedProviderRows"
            :row-props="(r: ProviderReportRow) => drillRowProps(r.provider_id != null ? { provider_id: r.provider_id } : null)"
            :loading="loading"
            :scroll-x="1030"
            :row-key="providerRowKey"
          >
            <template #empty>
              <EmptyState :icon="BarChart3" :title="t('analytics.noData')" />
            </template>
          </ResponsiveDataTable>
        </NTabPane>
        <NTabPane :name="'time'" :tab="t('analytics.dimensionTime')">
          <ResponsiveDataTable
            :columns="timeColumns"
            :data="timeRows"
            :row-props="(r: TimeReportRow) => drillRowProps(bucketRange(r.bucket))"
            :loading="loading"
            :scroll-x="1330"
            :row-key="(r: TimeReportRow) => r.bucket"
          >
            <template #empty>
              <EmptyState :icon="BarChart3" :title="t('analytics.noData')" />
            </template>
          </ResponsiveDataTable>
        </NTabPane>
        <NTabPane :name="'caller'" :tab="t('analytics.dimensionCaller')">
          <FilterSelectField
            v-if="isMobile"
            v-model:value="mobileSort"
            :label="t('analytics.mobileSortBy')"
            :options="mobileSortOptions"
            :placeholder="t('analytics.mobileSortDefault')"
            size="small"
            width="100%"
          />
          <ResponsiveDataTable
            :columns="callerColumns"
            :data="displayedCallerRows"
            :row-props="(r: CallerReportRow) => drillRowProps(r.api_key_id != null ? { api_key_id: r.api_key_id } : null)"
            :loading="loading"
            :scroll-x="1330"
            :row-key="callerRowKey"
          >
            <template #empty>
              <EmptyState :icon="BarChart3" :title="t('analytics.noData')" />
            </template>
          </ResponsiveDataTable>
        </NTabPane>
      </NTabs>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NSelect, NTabPane, NTabs, useMessage, type DataTableColumns, type SelectOption } from 'naive-ui'
import { BarChart3, Download } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import { useAuthStore } from '../../store/auth'
import { useIsMobile } from '../../composables/useIsMobile'
import { useRoute, useRouter } from 'vue-router'
import { bucketRange, requestLogLocation, type RequestLogLinkQuery } from '../../utils/requestLogLink'
import TimeRangeSelect, { type RangePreset, type TimeRange } from '../../components/analytics/TimeRangeSelect.vue'
import { listProviders } from '../../api/providers'
import { listModels } from '../../api/models'
import { listAPIKeys, toAPIKeyOptions } from '../../api/apiKeys'
import { useUserOptions } from '../../composables/useUserOptions'
import { clampedRangeStart, initialLast7DaysRange } from '../../utils/timeRange'
import { columnTitle } from '../../utils/columnTitle'
import { formatMicros } from '../../utils/money'
import { callerDisplay, formatNumber, formatRate } from '../../utils/format'
import {
  failoversColumn,
  avgDurationColumn,
  callsColumn,
  costColumn,
  successRateColumn,
  tokenColumn,
  unknownCostColumn,
} from '../../utils/analyticsColumns'
import { displayMessage } from '../../api/client'
import {
  exportAnalyticsCSV,
  getAnalyticsOverview,
  getAnalyticsReport,
  type AnalyticsBucket,
  type AnalyticsDimension,
  type AnalyticsFilter,
  type CallerReportRow,
  type UserReportRow,
  type ModelReportRow,
  type OverviewRow,
  type ProviderReportRow,
  type TimeReportRow,
} from '../../api/analytics'

const { t } = useI18n()
const message = useMessage()

// === Filter / dimension state =============================================

// Default window = last 7 days (matches the backend's default for
// dimension=time and feels like a reasonable default for "show me recent
// usage" without over-querying). Shared with the other dashboard pages via
// utils/timeRange.ts so every dashboard opens on the same window.
const authStore = useAuthStore()

// Account scope seeded from ?user_id= (dashboard drill-downs carry the
// active account filter through). Members never receive the param from
// our own links, and the backend pins them to themselves regardless.
const route = useRoute()
const seededUserID = Number(route.query.user_id)
const initialUserID = Number.isInteger(seededUserID) && seededUserID > 0 ? seededUserID : null

const preset = ref<RangePreset>('last7d')
const timeRange = ref<TimeRange>(initialLast7DaysRange())
const filter = ref<AnalyticsFilter>({ start: timeRange.value.start, end: timeRange.value.end, user_id: initialUserID })
// Admins land on the per-account view (the leftmost tab, and the "who is
// using how much" question they usually come for); members don't have that
// tab, so their default stays the model breakdown.
const dimension = ref<AnalyticsDimension>(authStore.isAdmin ? 'user' : 'model')
const bucket = ref<AnalyticsBucket>('day')

const bucketOptions = computed<SelectOption[]>(() => [
  { label: t('analytics.bucketDay'), value: 'day' },
  { label: t('analytics.bucketHour'), value: 'hour' },
])

// === Filter option lists ==================================================
//
// Inlined from the former AnalyticsFilterBar. These are admin-configured
// catalogs (not request-derived), so the lists are small and change
// infrequently — fetched once on mount, in parallel.
const apiKeyOptions = ref<SelectOption[]>([])
const providerOptions = ref<SelectOption[]>([])
const modelOptions = ref<SelectOption[]>([])
const { userOptions, loadUserOptions } = useUserOptions()

const statusOptions = computed<SelectOption[]>(() => [
  { label: t('analytics.statusSuccess'), value: 'success' },
  { label: t('analytics.statusFailed'), value: 'failed' },
  { label: t('analytics.statusPartial'), value: 'partial' },
  { label: t('analytics.statusCancelled'), value: 'cancelled' },
  { label: t('analytics.statusRejected'), value: 'rejected' },
])

// === Result state =========================================================
//
// Four dimension-typed refs instead of one `rows: unknown[]` because
// vue-tsc can't narrow a union through a single ref across renders — typed
// refs let the per-dimension DataTable bindings stay strict.

const overview = ref<OverviewRow | null>(null)
const modelRows = ref<ModelReportRow[]>([])
const providerRows = ref<ProviderReportRow[]>([])
const isMobile = useIsMobile()
const callerRows = ref<CallerReportRow[]>([])
// A report row that belongs to a real account. reload() narrows to this on the
// way in, which is what lets the row key and the drill-down below read
// user_id without a null case to handle.
type AttributedUserRow = UserReportRow & { user_id: number }
const userRows = ref<AttributedUserRow[]>([])
// Mobile-only sort preference, shared across the account/model/provider/caller tabs
// (the time tab stays chronological — reordering date buckets destroys the
// trend it exists to show). null keeps each dimension's server order: calls
// for model/provider, spend for caller.
const mobileSort = ref<'cost' | 'calls' | null>(null)
const mobileSortOptions = computed(() => [
  { label: t('analytics.costColumn'), value: 'cost' },
  { label: t('analytics.callsColumn'), value: 'calls' },
])
function mobileSorted<T extends { calls: number; cost_micros: number }>(rows: T[]): T[] {
  if (!isMobile.value || mobileSort.value === null) return rows
  const key = mobileSort.value
  return [...rows].sort((a, b) => (key === 'cost' ? b.cost_micros - a.cost_micros : b.calls - a.calls))
}
// Row drill-down: every report row opens the request-log list scoped to the
// page's active filter plus the clicked row's own dimension. Rows whose
// dimension value is the NULL bucket have nothing to scope by and stay inert.
const router = useRouter()
function activeFilterFragment(): RequestLogLinkQuery {
  // The report clamps aggregation to the 90-day day-bucket cap; the link
  // must cover the same window or the log list shows rows the clicked
  // aggregate never counted.
  const start = filter.value.start ?? null
  const end = filter.value.end ?? null
  return {
    start: start && end ? clampedRangeStart(start, end) : start,
    end,
    status: filter.value.status || null,
    model_name: filter.value.model_name || null,
    api_key_id: filter.value.api_key_id ?? null,
    user_id: filter.value.user_id ?? null,
    provider_id: filter.value.provider_id ?? null,
  }
}
// Plain pointer rows, matching the repo's existing row-click convention
// (models/providers/request-log lists): a button role on a <tr> would
// override its table-row semantics for screen readers. A keyboard-reachable
// form of row drill-down (a link cell) is a cross-page change tracked
// separately.
function drillRowProps(extra: RequestLogLinkQuery | null): Record<string, unknown> {
  // The drill target is the request-log audit page, which is admin-only —
  // a member click would just bounce off the router guard.
  if (extra === null || !authStore.isAdmin) return {}
  return {
    style: 'cursor: pointer',
    onClick: () => {
      void router.push(requestLogLocation({ ...activeFilterFragment(), ...extra }))
    },
  }
}

const displayedModelRows = computed(() => mobileSorted(modelRows.value))
const displayedProviderRows = computed(() => mobileSorted(providerRows.value))
const displayedCallerRows = computed(() => mobileSorted(callerRows.value))
const displayedUserRows = computed(() => mobileSorted(userRows.value))

// The overview cards obey the same filter as the table. Narrowed to one
// account or one key, accountless traffic falls outside both, the two agree,
// and the notice would be describing a gap that isn't there — so it is shown
// only while the filter bar leaves both of those unset.
const overviewCountsExcludedTraffic = computed(
  () => filter.value.user_id == null && filter.value.api_key_id == null,
)
const timeRows = ref<TimeReportRow[]>([])
const loading = ref(false)
const exporting = ref(false)

// reportRows is the dimension-agnostic accessor used by the export button's
// disabled state ("no rows to export" regardless of which tab is active).
const reportRows = computed<unknown[]>(() => {
  switch (dimension.value) {
    case 'model':
      return modelRows.value
    case 'provider':
      return providerRows.value
    case 'caller':
      return callerRows.value
    case 'user':
      return userRows.value
    case 'time':
      return timeRows.value
  }
})

// === Reload ===============================================================

// reloadSeq is a monotonic token guarding against stale reloads: a rapid
// filter/tab change starts a newer reload before the older one resolves, and
// without this guard the older response could land last and overwrite the
// newer overview/rows with stale data. Each reload captures its own seq and
// bails (without writing state) if a newer one has started.
let reloadSeq = 0

async function reload() {
  const mySeq = ++reloadSeq
  loading.value = true
  // Clear previous results IMMEDIATELY so a failed reload under new filters
  // can't leave stale financial data on screen.
  // The user sees a brief loading state rather than the previous filter's
  // numbers; on error the results stay cleared (not the stale values).
  overview.value = null
  modelRows.value = []
  providerRows.value = []
  callerRows.value = []
  userRows.value = []
  timeRows.value = []
  // Effective bucket: the time dimension honors the caller's bucket; every
  // other dimension uses 'day' for range resolution, so overview and non-time
  // reports clamp to the SAME cap (switching hour→model left overview
  // on the 30d hour cap while model used the 90d day cap).
  const effectiveBucket = dimension.value === 'time' ? bucket.value : 'day'
  // Two parallel round trips — overview and report are independent given
  // the same filter. Promise.all lets a single .catch surface either error.
  try {
    const [ov, report] = await Promise.all([
      getAnalyticsOverview(effectiveBucket, filter.value),
      getAnalyticsReport(dimension.value, bucket.value, filter.value, { withFailovers: dimension.value === 'provider' }),
    ])
    if (mySeq !== reloadSeq) return // a newer reload started; discard this one
    overview.value = ov
    // Narrow the untyped `rows: unknown` per dimension. The case set must
    // stay in sync with AnalyticsDimension for exhaustiveness — TS would
    // catch a missing case at compile time via the function's return type.
    switch (report.dimension) {
      case 'model':
        modelRows.value = (report.rows as ModelReportRow[]) ?? []
        // Members can't read the admin model catalog; the models they've
        // actually used are the only ones worth filtering by anyway.
        if (!authStore.isAdmin) {
          modelOptions.value = modelRows.value
            .filter((r) => r.model_name)
            .map((r) => ({ label: r.model_name, value: r.model_name }))
        }
        break
      case 'provider':
        providerRows.value = (report.rows as ProviderReportRow[]) ?? []
        break
      case 'caller':
        callerRows.value = (report.rows as CallerReportRow[]) ?? []
        break
      case 'user':
        // The server already leaves the accountless bucket out. This repeats
        // it because a rolling upgrade can serve this bundle from an older
        // binary that still sends the row, and this report is defined as
        // per-account: a row belonging to no account has no place in it, and
        // showing one would also contradict the notice above the table.
        // Narrowing here rather than testing for null at each use is what
        // keeps the rest of the page free of a case that cannot occur.
        // The CSV export is a browser navigation straight to the server, so
        // this cannot cover it; an older binary's export still carries the
        // row until every instance is upgraded.
        userRows.value = ((report.rows as UserReportRow[]) ?? []).filter(
          (r): r is AttributedUserRow => r.user_id != null,
        )
        break
      case 'time':
        timeRows.value = (report.rows as TimeReportRow[]) ?? []
        break
    }
  } catch (err) {
    if (mySeq !== reloadSeq) return
    message.error(displayMessage(err, t))
    // overview/rows stay cleared (set above) — no stale data under new filter.
  } finally {
    // Only clear loading when the latest reload finishes — otherwise a stale
    // finally could flip it to false while the newer reload is still in flight.
    if (mySeq === reloadSeq) loading.value = false
  }
}

onMounted(() => {
  void reload()
  void loadFilterOptions()
})

// Fetch the filter selectors' option lists once. Failure is degraded, not
// broken — the user can still type a model name; show the error inline but
// don't block the page.
async function loadFilterOptions() {
  try {
    // Members only get the key selector: the provider/model/user catalogs
    // are admin-only endpoints, and the corresponding filters are hidden
    // from their filter bar anyway (model options are derived from their
    // own report rows instead, below).
    if (!authStore.isAdmin) {
      const apiKeyPage = await listAPIKeys({ q: '', status: '', page: 1, pageSize: 200 })
      apiKeyOptions.value = toAPIKeyOptions(apiKeyPage.list)
      return
    }
    // loadUserOptions assigns and toasts internally; riding in the same
    // Promise.all keeps all four catalogs loading in parallel.
    const [providerPage, modelPage, apiKeyPage] = await Promise.all([
      listProviders(),
      listModels(),
      listAPIKeys({ q: '', status: '', page: 1, pageSize: 200 }),
      loadUserOptions(),
    ])
    providerOptions.value = providerPage.list.map((p) => ({ label: p.name, value: p.id }))
    modelOptions.value = modelPage.list.map((m) => ({ label: m.name, value: m.name }))
    apiKeyOptions.value = toAPIKeyOptions(apiKeyPage.list)
  } catch (err) {
    message.error(displayMessage(err, t))
  }
}

// Reload whenever the dimension / bucket / filter changes. The watch is
// deep on `filter` because filter changes always emit a new object (see
// update()).
watch([dimension, bucket, filter], () => {
  void reload()
}, { deep: true })

// === Event handlers =======================================================

// Merge a single filter field, always emitting a new object so the deep
// watch fires and reloads.
function update<K extends keyof AnalyticsFilter>(key: K, value: AnalyticsFilter[K]) {
  filter.value = { ...filter.value, [key]: value }
}

function onTimeRange(v: TimeRange) {
  timeRange.value = v
  filter.value = { ...filter.value, start: v.start, end: v.end }
}

function onPreset(v: RangePreset) {
  preset.value = v
}

function onDimensionChange(v: string | number) {
  // NTabs emits string | number; we know our tab names are the dimension
  // strings. The cast is safe because the tabs are statically defined.
  dimension.value = v as AnalyticsDimension
}

function onBucketChange(v: AnalyticsBucket) {
  bucket.value = v
}

function onExport() {
  exporting.value = true
  try {
    exportAnalyticsCSV(dimension.value, bucket.value, filter.value)
  } finally {
    // The export is a navigation click, not a promise — there's nothing to
    // await. The toggle just covers the brief moment between mousedown and
    // the browser's download dialog.
    setTimeout(() => {
      exporting.value = false
    }, 600)
  }
}

// === Row keys =============================================================
//
// The provider/caller dimensions include a synthetic bucket for rows with
// NULL provider_id / api_key_id (auth failed before routing, etc.). naive-ui
// needs a unique string row-key; fall back to a fixed sentinel for those
// NULL rows so they're still selectable / paginated correctly. The account
// dimension has no such bucket to key around.

function providerRowKey(r: ProviderReportRow): string {
  return r.provider_id == null ? '__null_provider__' : `p-${r.provider_id}`
}

// No sentinel here, unlike the two above: this dimension's rows are narrowed
// to real accounts before they reach the table, so the id is always present.
function userRowKey(r: AttributedUserRow): string {
  return String(r.user_id)
}

function callerRowKey(r: CallerReportRow): string {
  return r.api_key_id == null ? '__null_caller__' : `k-${r.api_key_id}`
}

// === Column definitions ===================================================
//
// Dimension (label) columns stay inline here because each is specific to a
// dimension (model name / provider name / caller / bucket). The shared metric
// columns (calls / successRate / cost / unknownCost / tokens / avgDuration)
// come from utils/analyticsColumns.ts so a metric column change lands in every
// dimension at once instead of being copy-pasted across four column arrays.

const modelColumns = computed<DataTableColumns<ModelReportRow>>(() => [
  {
    title: columnTitle(t('analytics.modelNameColumn'), t('analytics.modelNameColumn_tip')),
    key: 'model_name',
    minWidth: 200,
    render: (r) => h('span', { class: 'mono-cell' }, r.model_name || '—'),
  },
  callsColumn<ModelReportRow>(t, { sortable: true }),
  successRateColumn<ModelReportRow>(t),
  tokenColumn<ModelReportRow>(t, 'input_tokens', 'inputTokensColumn'),
  tokenColumn<ModelReportRow>(t, 'output_tokens', 'outputTokensColumn'),
  tokenColumn<ModelReportRow>(t, 'cache_write_tokens', 'cacheWriteTokensColumn', 150),
  tokenColumn<ModelReportRow>(t, 'cache_read_tokens', 'cacheReadTokensColumn', 150),
  costColumn<ModelReportRow>(t, { sortable: true }),
  unknownCostColumn<ModelReportRow>(t),
])

const providerColumns = computed<DataTableColumns<ProviderReportRow>>(() => [
  {
    title: columnTitle(t('analytics.providerNameColumn'), t('analytics.providerNameColumn_tip')),
    key: 'provider_name',
    minWidth: 200,
    render: (r) => r.provider_name || t('analytics.unroutedBucket'),
  },
  callsColumn<ProviderReportRow>(t, { sortable: true }),
  successRateColumn<ProviderReportRow>(t),
  failoversColumn<ProviderReportRow>(t),
  avgDurationColumn<ProviderReportRow>(t),
  costColumn<ProviderReportRow>(t, { sortable: true }),
  unknownCostColumn<ProviderReportRow>(t),
])

const callerColumns = computed<DataTableColumns<CallerReportRow>>(() => [
  {
    title: columnTitle(t('analytics.callerColumn'), t('analytics.callerColumn_tip')),
    key: 'username',
    minWidth: 200,
    render: (r) => callerDisplay(r.username, r.key_prefix) || t('analytics.unknownCallerBucket'),
  },
  // The ranking leads with spend (the server orders by it); the calls
  // column stays sortable so the old by-volume ordering remains reachable
  // from the header.
  callsColumn<CallerReportRow>(t, { sortable: true }),
  successRateColumn<CallerReportRow>(t),
  tokenColumn<CallerReportRow>(t, 'input_tokens', 'inputTokensColumn'),
  tokenColumn<CallerReportRow>(t, 'output_tokens', 'outputTokensColumn'),
  tokenColumn<CallerReportRow>(t, 'cache_write_tokens', 'cacheWriteTokensColumn', 150),
  tokenColumn<CallerReportRow>(t, 'cache_read_tokens', 'cacheReadTokensColumn', 150),
  costColumn<CallerReportRow>(t, { sortable: true, defaultDescend: true }),
  unknownCostColumn<CallerReportRow>(t),
])

const userColumns = computed<DataTableColumns<AttributedUserRow>>(() => [
  {
    title: columnTitle(t('analytics.userColumn'), t('analytics.userColumn_tip')),
    key: 'username',
    minWidth: 200,
    render: (r) => r.username || '—',
  },
  callsColumn<AttributedUserRow>(t, { sortable: true }),
  successRateColumn<AttributedUserRow>(t),
  tokenColumn<AttributedUserRow>(t, 'input_tokens', 'inputTokensColumn'),
  tokenColumn<AttributedUserRow>(t, 'output_tokens', 'outputTokensColumn'),
  tokenColumn<AttributedUserRow>(t, 'cache_write_tokens', 'cacheWriteTokensColumn', 150),
  tokenColumn<AttributedUserRow>(t, 'cache_read_tokens', 'cacheReadTokensColumn', 150),
  costColumn<AttributedUserRow>(t, { sortable: true, defaultDescend: true }),
  unknownCostColumn<AttributedUserRow>(t),
])

const timeColumns = computed<DataTableColumns<TimeReportRow>>(() => [
  {
    title: columnTitle(t('analytics.bucketColumn'), t('analytics.bucketColumn_tip')),
    key: 'bucket',
    minWidth: 180,
    render: (r) => h('span', { class: 'mono-cell' }, r.bucket),
  },
  callsColumn<TimeReportRow>(t),
  successRateColumn<TimeReportRow>(t),
  tokenColumn<TimeReportRow>(t, 'input_tokens', 'inputTokensColumn'),
  tokenColumn<TimeReportRow>(t, 'output_tokens', 'outputTokensColumn'),
  tokenColumn<TimeReportRow>(t, 'cache_write_tokens', 'cacheWriteTokensColumn', 150),
  tokenColumn<TimeReportRow>(t, 'cache_read_tokens', 'cacheReadTokensColumn', 150),
  costColumn<TimeReportRow>(t),
  unknownCostColumn<TimeReportRow>(t),
])
</script>

<style scoped>
.bucket-bar {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.unattributed-note {
  margin: 0 0 var(--space-3);
  font-size: var(--text-xs);
  color: var(--color-text-muted);
}

.bucket-label {
  font-size: var(--text-xs);
  font-weight: 700;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--color-text-muted);
}

.metric-row {
  display: grid;
  grid-template-columns: repeat(5, 1fr);
  gap: var(--space-4);
}

.metric {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: var(--space-4);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
}

.metric__label {
  font-size: var(--text-xs);
  font-weight: 700;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--color-text-muted);
}

.metric__value {
  font-size: 1.5rem;
  font-weight: 800;
  line-height: 1;
  font-variant-numeric: tabular-nums;
  color: var(--color-text);
}

.metric__sub {
  font-size: var(--text-xs);
  color: var(--color-text-muted);
}

.section-card {
  padding: var(--space-5);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
}

:deep(.mono-cell) {
  font-family: var(--font-mono, monospace);
  font-weight: 600;
  color: var(--color-text);
}

@media (max-width: 1100px) {
  .metric-row {
    grid-template-columns: repeat(2, 1fr);
  }
}
@media (max-width: 1100px) {
  .section-card.table-card {
    padding: 0;
    border: 0;
  }

  :deep(.n-tabs-nav-scroll-wrapper) {
    padding: 0 20px;
    border: 1px solid var(--color-border-subtle);
    border-radius: var(--radius-lg);
  }
}
</style>
