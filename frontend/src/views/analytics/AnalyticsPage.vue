<!-- frontend/src/views/analytics/AnalyticsPage.vue
     Usage report. Combines:
       - Filter bar (time range / api key / model / provider / status)
       - Dimension tabs (model / provider / time / caller)
       - Overview metric row (calls / success rate / tokens / cost)
       - Dimension-specific NDataTable (column tooltips via columnTitle)
       - CSV export button

     The page owns the filter + dimension state. Each change triggers a
     reload of both /overview and /report in parallel — they're independent
     given the same filter, so a single error message covers both. -->
<template>
  <div class="analytics-page">
    <PageHeader :eyebrow="t('analytics.eyebrow')" :title="t('analytics.pageTitle')" :description="t('analytics.pageDescription')">
      <template #actions>
        <NButton :loading="exporting" :disabled="!reportRows.length" @click="onExport">
          <template #icon><Download :size="16" /></template>
          {{ t('analytics.exportCSV') }}
        </NButton>
      </template>
    </PageHeader>

    <AnalyticsFilterBar
      :filter="filter"
      :time-range="timeRange"
      :preset="preset"
      @update:filter="onFilterChange"
      @update:time-range="onTimeRangeChange"
      @update:preset="onPresetChange"
    />

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
        <div class="metric__value">¥{{ formatMicros(overview?.cost_micros ?? 0) }}</div>
        <div v-if="(overview?.unknown_cost_calls ?? 0) > 0" class="metric__sub">
          {{ t('analytics.unknownCostSub', { n: overview?.unknown_cost_calls ?? 0 }) }}
        </div>
      </div>
    </div>

    <!-- Dimension tabs + report table -->
    <div class="section-card">
      <NTabs :value="dimension" type="line" @update:value="onDimensionChange">
        <NTabPane :name="'model'" :tab="t('analytics.dimensionModel')">
          <NDataTable
            :columns="modelColumns"
            :data="modelRows"
            :loading="loading"
            :bordered="false"
            :single-line="false"
            :row-key="(r: ModelReportRow) => r.model_name"
            size="small"
          >
            <template #empty>
              <EmptyState :title="t('analytics.noData')" />
            </template>
          </NDataTable>
        </NTabPane>
        <NTabPane :name="'provider'" :tab="t('analytics.dimensionProvider')">
          <NDataTable
            :columns="providerColumns"
            :data="providerRows"
            :loading="loading"
            :bordered="false"
            :single-line="false"
            :row-key="providerRowKey"
            size="small"
          >
            <template #empty>
              <EmptyState :title="t('analytics.noData')" />
            </template>
          </NDataTable>
        </NTabPane>
        <NTabPane :name="'time'" :tab="t('analytics.dimensionTime')">
          <NDataTable
            :columns="timeColumns"
            :data="timeRows"
            :loading="loading"
            :bordered="false"
            :single-line="false"
            :row-key="(r: TimeReportRow) => r.bucket"
            size="small"
          >
            <template #empty>
              <EmptyState :title="t('analytics.noData')" />
            </template>
          </NDataTable>
        </NTabPane>
        <NTabPane :name="'caller'" :tab="t('analytics.dimensionCaller')">
          <NDataTable
            :columns="callerColumns"
            :data="callerRows"
            :loading="loading"
            :bordered="false"
            :single-line="false"
            :row-key="callerRowKey"
            size="small"
          >
            <template #empty>
              <EmptyState :title="t('analytics.noData')" />
            </template>
          </NDataTable>
        </NTabPane>
      </NTabs>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NDataTable, NTabPane, NTabs, useMessage, type DataTableColumns, type SelectOption } from 'naive-ui'
import { Download } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import AnalyticsFilterBar from '../../components/analytics/AnalyticsFilterBar.vue'
import { type RangePreset, type TimeRange } from '../../components/analytics/TimeRangeSelect.vue'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { formatMicros } from '../../utils/money'
import { displayMessage } from '../../api/client'
import {
  exportAnalyticsCSV,
  getAnalyticsOverview,
  getAnalyticsReport,
  type AnalyticsBucket,
  type AnalyticsDimension,
  type AnalyticsFilter,
  type CallerReportRow,
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
// usage" without over-querying).
const initialRange = (): TimeRange => {
  const now = new Date()
  const end = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1, 0, 0, 0, 0)
  const start = new Date(end)
  start.setDate(start.getDate() - 7)
  return { start: start.toISOString(), end: end.toISOString() }
}

const preset = ref<RangePreset>('last7d')
const timeRange = ref<TimeRange>(initialRange())
const filter = ref<AnalyticsFilter>({ start: timeRange.value.start, end: timeRange.value.end })
const dimension = ref<AnalyticsDimension>('model')
const bucket = ref<AnalyticsBucket>('day')

const bucketOptions = computed<SelectOption[]>(() => [
  { label: t('analytics.bucketDay'), value: 'day' },
  { label: t('analytics.bucketHour'), value: 'hour' },
])

// === Result state =========================================================
//
// Four dimension-typed refs instead of one `rows: unknown[]` because
// vue-tsc can't narrow a union through a single ref across renders — typed
// refs let the per-dimension DataTable bindings stay strict.

const overview = ref<OverviewRow | null>(null)
const modelRows = ref<ModelReportRow[]>([])
const providerRows = ref<ProviderReportRow[]>([])
const callerRows = ref<CallerReportRow[]>([])
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
      getAnalyticsReport(dimension.value, bucket.value, filter.value),
    ])
    if (mySeq !== reloadSeq) return // a newer reload started; discard this one
    overview.value = ov
    // Narrow the untyped `rows: unknown` per dimension. The case set must
    // stay in sync with AnalyticsDimension for exhaustiveness — TS would
    // catch a missing case at compile time via the function's return type.
    switch (report.dimension) {
      case 'model':
        modelRows.value = (report.rows as ModelReportRow[]) ?? []
        break
      case 'provider':
        providerRows.value = (report.rows as ProviderReportRow[]) ?? []
        break
      case 'caller':
        callerRows.value = (report.rows as CallerReportRow[]) ?? []
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
})

// Reload whenever the dimension / bucket / filter changes. The watch is
// deep on `filter` because filter changes always emit a new object (see
// AnalyticsFilterBar.update).
watch([dimension, bucket, filter], () => {
  void reload()
}, { deep: true })

// === Event handlers =======================================================

function onFilterChange(v: AnalyticsFilter) {
  filter.value = v
}

function onTimeRangeChange(v: TimeRange) {
  timeRange.value = v
}

function onPresetChange(v: RangePreset) {
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

// === Formatters ===========================================================

function formatNumber(n: number): string {
  return n.toLocaleString()
}

function formatRate(r: number): string {
  return `${(r * 100).toFixed(1)}%`
}

// === Row keys for NULL-id buckets =========================================
//
// The provider/caller dimensions include a synthetic bucket for rows with
// NULL provider_id / api_key_id (auth failed before routing, etc.). naive-ui
// needs a unique string row-key; fall back to a fixed sentinel for those
// NULL rows so they're still selectable / paginated correctly.

function providerRowKey(r: ProviderReportRow): string {
  return r.provider_id == null ? '__null_provider__' : `p-${r.provider_id}`
}

function callerRowKey(r: CallerReportRow): string {
  return r.api_key_id == null ? '__null_caller__' : `k-${r.api_key_id}`
}

// === Column definitions ===================================================
//
// The four dimension tables share the same metric columns (calls /
// successRate / cost / unknownCost, plus input/output tokens except for
// provider). The factories below define each metric column ONCE so a column
// change (label, width, format) lands in every dimension at once instead of
// being copy-pasted across four DataTableColumns definitions — which had
// already drifted (unknown_cost width was 150 in model/time, 140 in
// provider/caller).

type MetricRow = {
  calls: number
  success_rate: number
  cost_micros: number
  unknown_cost_calls: number
}

function callsColumn<T extends MetricRow>(): DataTableColumns<T>[number] {
  return {
    title: columnTitle(t('analytics.callsColumn'), t('analytics.callsColumn_tip')),
    key: 'calls',
    width: 120,
    align: 'right',
    render: (r: T) => formatNumber(r.calls),
  }
}
function successRateColumn<T extends MetricRow>(): DataTableColumns<T>[number] {
  return {
    title: columnTitle(t('analytics.successRateColumn'), t('analytics.successRateColumn_tip')),
    key: 'success_rate',
    width: STATUS_COL_WIDTH,
    align: 'right',
    render: (r: T) => formatRate(r.success_rate),
  }
}
function costColumn<T extends MetricRow>(): DataTableColumns<T>[number] {
  return {
    title: columnTitle(t('analytics.costColumn'), t('analytics.costColumn_tip')),
    key: 'cost_micros',
    width: 140,
    align: 'right',
    render: (r: T) => `¥${formatMicros(r.cost_micros)}`,
  }
}
function unknownCostColumn<T extends MetricRow>(): DataTableColumns<T>[number] {
  return {
    title: columnTitle(t('analytics.unknownCostColumn'), t('analytics.unknownCostColumn_tip')),
    key: 'unknown_cost_calls',
    width: 140,
    align: 'right',
    render: (r: T) => formatNumber(r.unknown_cost_calls),
  }
}
// tokenColumn is the shared factory for every right-aligned token count
// (input / output / cache write / cache read). i18nKey names both the header
// (`analytics.<i18nKey>`) and its tooltip (`analytics.<i18nKey>_tip`); the
// column key is the row field to render. Replaces four near-identical factories.
function tokenColumn<T extends MetricRow>(
  key: keyof T & string,
  i18nKey: string,
  width = 140,
): DataTableColumns<T>[number] {
  return {
    title: columnTitle(t(`analytics.${i18nKey}`), t(`analytics.${i18nKey}_tip`)),
    key,
    width,
    align: 'right',
    render: (r: T) => formatNumber(r[key] as number),
  }
}

const modelColumns = computed<DataTableColumns<ModelReportRow>>(() => [
  {
    title: columnTitle(t('analytics.modelNameColumn'), t('analytics.modelNameColumn_tip')),
    key: 'model_name',
    minWidth: 200,
    render: (r) => h('span', { class: 'mono-cell' }, r.model_name || '—'),
  },
  callsColumn<ModelReportRow>(),
  successRateColumn<ModelReportRow>(),
  tokenColumn<ModelReportRow>('input_tokens', 'inputTokensColumn'),
  tokenColumn<ModelReportRow>('output_tokens', 'outputTokensColumn'),
  tokenColumn<ModelReportRow>('cache_write_tokens', 'cacheWriteTokensColumn', 150),
  tokenColumn<ModelReportRow>('cache_read_tokens', 'cacheReadTokensColumn', 150),
  costColumn<ModelReportRow>(),
  unknownCostColumn<ModelReportRow>(),
])

const providerColumns = computed<DataTableColumns<ProviderReportRow>>(() => [
  {
    title: columnTitle(t('analytics.providerNameColumn'), t('analytics.providerNameColumn_tip')),
    key: 'provider_name',
    minWidth: 200,
    render: (r) => r.provider_name || t('analytics.unroutedBucket'),
  },
  callsColumn<ProviderReportRow>(),
  successRateColumn<ProviderReportRow>(),
  {
    title: columnTitle(t('analytics.avgDurationColumn'), t('analytics.avgDurationColumn_tip')),
    key: 'avg_duration_ms',
    width: 140,
    align: 'right',
    render: (r) => `${r.avg_duration_ms.toFixed(0)}ms`,
  },
  costColumn<ProviderReportRow>(),
  unknownCostColumn<ProviderReportRow>(),
])

const callerColumns = computed<DataTableColumns<CallerReportRow>>(() => [
  {
    title: columnTitle(t('analytics.callerColumn'), t('analytics.callerColumn_tip')),
    key: 'owner_label',
    minWidth: 200,
    render: (r) => r.owner_label || t('analytics.unknownCallerBucket'),
  },
  callsColumn<CallerReportRow>(),
  successRateColumn<CallerReportRow>(),
  tokenColumn<CallerReportRow>('input_tokens', 'inputTokensColumn'),
  tokenColumn<CallerReportRow>('output_tokens', 'outputTokensColumn'),
  tokenColumn<CallerReportRow>('cache_write_tokens', 'cacheWriteTokensColumn', 150),
  tokenColumn<CallerReportRow>('cache_read_tokens', 'cacheReadTokensColumn', 150),
  costColumn<CallerReportRow>(),
  unknownCostColumn<CallerReportRow>(),
])

const timeColumns = computed<DataTableColumns<TimeReportRow>>(() => [
  {
    title: columnTitle(t('analytics.bucketColumn'), t('analytics.bucketColumn_tip')),
    key: 'bucket',
    minWidth: 180,
    render: (r) => h('span', { class: 'mono-cell' }, r.bucket),
  },
  callsColumn<TimeReportRow>(),
  successRateColumn<TimeReportRow>(),
  tokenColumn<TimeReportRow>('input_tokens', 'inputTokensColumn'),
  tokenColumn<TimeReportRow>('output_tokens', 'outputTokensColumn'),
  tokenColumn<TimeReportRow>('cache_write_tokens', 'cacheWriteTokensColumn', 150),
  tokenColumn<TimeReportRow>('cache_read_tokens', 'cacheReadTokensColumn', 150),
  costColumn<TimeReportRow>(),
  unknownCostColumn<TimeReportRow>(),
])
</script>

<style scoped>
.analytics-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

.bucket-bar {
  display: flex;
  align-items: center;
  gap: var(--space-2);
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

@media (max-width: 640px) {
  .metric-row {
    grid-template-columns: 1fr;
  }
}
</style>
