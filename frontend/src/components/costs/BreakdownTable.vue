<!-- frontend/src/components/costs/BreakdownTable.vue
     Dimension breakdown table for the per-entity cost detail pages. Renders
     one row per model / provider / caller bucket with the shared metric
     columns (calls, success rate, tokens or avg duration, cache hit rate,
     cache net saving, cost, unknown cost).

     Presentation-only: clicking a dimension row emits `select` with the row's
     entity identity; the PARENT page owns router.push so routing lives in one
     place. Rows whose identity is missing (empty model_name, NULL provider_id,
     NULL api_key_id — the "unrouted" / "no caller" buckets) render as plain
     non-clickable text. -->
<template>
  <ResponsiveDataTable
    :columns="columns"
    :data="dataRows"
    :loading="loading"
    :scroll-x="1600"
    :row-key="rowKey"
  >
    <template #empty>
      <EmptyState :icon="TableIcon" :title="t('costs.noData')" />
    </template>
  </ResponsiveDataTable>
</template>

<script setup lang="ts">
import { computed, h } from 'vue'
import { callerDisplay } from '../../utils/format'
import { useI18n } from 'vue-i18n'
import { type DataTableColumns } from 'naive-ui'
import { Table as TableIcon } from '@lucide/vue'
import EmptyState from '../EmptyState.vue'
import ResponsiveDataTable from '../common/ResponsiveDataTable.vue'
import { columnTitle } from '../../utils/columnTitle'
import {
  avgDurationColumn,
  cacheHitRateColumn,
  cacheNetSavedColumn,
  callsColumn,
  costColumn,
  successRateColumn,
  tokenColumn,
  unknownCostColumn,
} from '../../utils/analyticsColumns'
import type {
  CallerReportRow,
  ModelReportRow,
  ProviderReportRow,
} from '../../api/analytics'

type Dimension = 'model' | 'provider' | 'caller'

// Union row type. The prop accepts the per-dimension array shapes; internally
// we widen to BreakdownRow so one column array type covers every dimension.
type BreakdownRow = ModelReportRow | ProviderReportRow | CallerReportRow

const props = defineProps<{
  rows: ModelReportRow[] | ProviderReportRow[] | CallerReportRow[]
  dimension: Dimension
  loading?: boolean
}>()

const emit = defineEmits<{
  select: [payload: { kind: Dimension; model?: string; providerId?: number; apiKeyId?: number }]
}>()

const { t } = useI18n()

// NDataTable's data binding expects a stable array type; the rows prop is a
// union of three array shapes (one per dimension), so widen to BreakdownRow[].
// At runtime the parent always passes the array matching the current dimension.
const dataRows = computed<BreakdownRow[]>(() => props.rows as BreakdownRow[])

// rowKey gives each row a unique stable id so NDataTable can paginate and
// track selection. NULL-id buckets fall back to fixed sentinels (mirroring
// AnalyticsPage's row-key strategy).
function rowKey(row: BreakdownRow): string {
  if (props.dimension === 'model') {
    return (row as ModelReportRow).model_name || '__empty_model__'
  }
  if (props.dimension === 'provider') {
    const id = (row as ProviderReportRow).provider_id
    return id == null ? '__null_provider__' : `p-${id}`
  }
  const id = (row as CallerReportRow).api_key_id
  return id == null ? '__null_caller__' : `k-${id}`
}

// A row is clickable only when it carries a concrete identity:
//   - model: model_name is non-empty (the "" bucket is the unrouted fallback)
//   - provider: provider_id is non-null (NULL = rejected before routing)
//   - caller: api_key_id is non-null (NULL = no associated key)
function isClickable(row: BreakdownRow): boolean {
  if (props.dimension === 'model') return (row as ModelReportRow).model_name !== ''
  if (props.dimension === 'provider') return (row as ProviderReportRow).provider_id != null
  return (row as CallerReportRow).api_key_id != null
}

function emitSelect(row: BreakdownRow): void {
  if (!isClickable(row)) return
  if (props.dimension === 'model') {
    emit('select', { kind: 'model', model: (row as ModelReportRow).model_name })
  } else if (props.dimension === 'provider') {
    const id = (row as ProviderReportRow).provider_id
    if (id != null) emit('select', { kind: 'provider', providerId: id })
  } else {
    const id = (row as CallerReportRow).api_key_id
    if (id != null) emit('select', { kind: 'caller', apiKeyId: id })
  }
}

// Cast helper: naive-ui column types are contravariant on the row type
// (render receives a row), so a column built for ModelReportRow is not
// directly assignable to one for BreakdownRow. The runtime contract holds
// because dimension-scoped columns only ever render dimension-matching rows.
type BreakdownCol = DataTableColumns<BreakdownRow>[number]
function asBreakdownCol<T>(col: DataTableColumns<T>[number]): BreakdownCol {
  return col as unknown as BreakdownCol
}

const columns = computed<DataTableColumns<BreakdownRow>>(() => {
  const isModel = props.dimension === 'model'
  const isProvider = props.dimension === 'provider'

  const labelKey = isModel
    ? 'analytics.modelNameColumn'
    : isProvider
      ? 'analytics.providerNameColumn'
      : 'analytics.callerColumn'
  const tipKey = isModel
    ? 'analytics.modelNameColumn_tip'
    : isProvider
      ? 'analytics.providerNameColumn_tip'
      : 'analytics.callerColumn_tip'

  const dimCol: BreakdownCol = {
    title: columnTitle(t(labelKey), t(tipKey)),
    key: 'dim',
    minWidth: 200,
    render: (row) => {
      const label = isModel
        ? (row as ModelReportRow).model_name || '—'
        : isProvider
          ? (row as ProviderReportRow).provider_name || t('analytics.unroutedBucket')
          : callerDisplay((row as CallerReportRow).username, (row as CallerReportRow).key_prefix) || t('analytics.unknownCallerBucket')
      if (!isClickable(row)) return label
      // Clickable link styling; emit on click (router.push handled by parent
      // to keep this component presentation-only).
      return h(
        'a',
        {
          class: 'breakdown-link',
          href: '#',
          onClick: (e: MouseEvent) => {
            e.preventDefault()
            emitSelect(row)
          },
        },
        label,
      )
    },
  }

  // Per-dimension column sets mirror AnalyticsPage:
  //   provider → [dim, calls, successRate, avgDuration, cacheHitRate,
  //               cacheNetSaved, cost, unknownCost]
  //   model/caller → [dim, calls, successRate, input, output, cacheWrite,
  //                    cacheRead, cacheHitRate, cacheNetSaved, cost,
  //                    unknownCost]
  // The four token-count columns are omitted for the provider dimension
  // (avgDuration takes their slot) to keep that table compact; the two cache
  // economics columns stay, since provider rows carry the sums they need.
  if (isProvider) {
    return [
      dimCol,
      asBreakdownCol(callsColumn<ProviderReportRow>(t)),
      asBreakdownCol(successRateColumn<ProviderReportRow>(t)),
      asBreakdownCol(avgDurationColumn<ProviderReportRow>(t)),
      asBreakdownCol(cacheHitRateColumn<ProviderReportRow>(t)),
      asBreakdownCol(cacheNetSavedColumn<ProviderReportRow>(t)),
      asBreakdownCol(costColumn<ProviderReportRow>(t)),
      asBreakdownCol(unknownCostColumn<ProviderReportRow>(t)),
    ]
  }
  return [
    dimCol,
    asBreakdownCol(callsColumn<ModelReportRow>(t)),
    asBreakdownCol(successRateColumn<ModelReportRow>(t)),
    asBreakdownCol(tokenColumn<ModelReportRow>(t, 'input_tokens', 'inputTokensColumn')),
    asBreakdownCol(tokenColumn<ModelReportRow>(t, 'output_tokens', 'outputTokensColumn')),
    asBreakdownCol(tokenColumn<ModelReportRow>(t, 'cache_write_tokens', 'cacheWriteTokensColumn', 150)),
    asBreakdownCol(tokenColumn<ModelReportRow>(t, 'cache_read_tokens', 'cacheReadTokensColumn', 150)),
    asBreakdownCol(cacheHitRateColumn<ModelReportRow>(t)),
    asBreakdownCol(cacheNetSavedColumn<ModelReportRow>(t)),
    asBreakdownCol(costColumn<ModelReportRow>(t)),
    asBreakdownCol(unknownCostColumn<ModelReportRow>(t)),
  ]
})
</script>

<style scoped>
.breakdown-link {
  color: var(--color-primary);
  cursor: pointer;
  text-decoration: none;
}

.breakdown-link:hover {
  text-decoration: underline;
}
</style>
