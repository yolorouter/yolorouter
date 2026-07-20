<!-- frontend/src/components/dashboard/TrendChart.vue
     Dashboard "calls / cost over the last N days" trend (PRD §6.6.3).
     Rendered with ECharts (bar + line, dual Y axis) via `vue-echarts`.
     Registration is centralized in `utils/echarts.ts` — importing it once
     here is enough; `VChart` is re-exported from there so a single import
     gives us both the component and the registration side-effect. -->
<template>
  <div class="trend-chart">
    <div v-if="!points.length" class="trend-empty">{{ t('analytics.noData') }}</div>
    <VChart
      v-else
      class="trend-vchart"
      :option="option"
      :update-options="{ notMerge: false }"
      autoresize
    />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
// Single import registers every chart type / component we need (side effect)
// and gives us the VChart component to render with.
import { VChart } from '../../utils/echarts'
import { formatCents } from '../../utils/money'
import type { TrendPoint } from '../../api/analytics'

const props = defineProps<{ points: TrendPoint[] }>()
const { t } = useI18n()

// ECharts does NOT resolve CSS custom properties in itemStyle.color — the
// renderer expects an actual color string. We pin the two brand colors here
// to hex literals that match the tokens in `styles/tokens.less`:
//   --color-accent: #6467f2       (see tokens.less:36)
//   --color-purple: oklch(58% 0.16 300) — approximated as #8b5cf6 (a hex
//                   equivalent close enough for canvas rendering).
const ACCENT = '#6467f2'
const PURPLE = '#8b5cf6'

const TEXT_MUTED = '#909399'
const GRID_LINE = '#f0f0f3'
const AXIS_LINE = '#e0e0e6'

// "YYYY-MM-DD" -> "MM-DD" for the x-axis tick, so a 7-bar trend fits.
function formatAxisDate(s: string): string {
  return s.length >= 10 ? s.slice(5) : s
}

const option = computed(() => {
  const dates = props.points.map((p) => formatAxisDate(p.date))
  const calls = props.points.map((p) => p.calls)
  // Cost is shown in yuan (major unit); cost_cents/100 — same formatter as
  // the table column, so axis ticks and tooltip stay consistent with it.
  const costs = props.points.map((p) => Number(formatCents(p.cost_cents)))

  return {
    // `axisPointer` lets the tooltip snap to the column under the cursor
    // rather than the exact pixel, so hovering the bar OR the line shows the
    // same card. A global formatter is used instead of valueFormatter because
    // echarts passes dataIndex (not seriesIndex) as valueFormatter's second
    // arg, so per-series currency formatting has to branch on params[i].
    // seriesIndex here instead.
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'shadow' },
      formatter: (params: unknown) => {
        if (!Array.isArray(params) || !params.length) return ''
        const first = params[0] as { axisValueLabel?: string; name?: string }
        const header = first.axisValueLabel || first.name || ''
        const lines = params.map((p: { seriesIndex: number; seriesName: string; marker: string; value: unknown }) => {
          const isCost = p.seriesIndex === 1
          const val = isCost ? `¥${Number(p.value).toFixed(2)}` : String(p.value)
          return `${p.marker} ${p.seriesName}: ${val}`
        })
        return [header, ...lines].join('<br/>')
      },
    },
    legend: {
      bottom: 0,
      icon: 'roundRect',
      itemWidth: 14,
      itemHeight: 4,
      // Pin the legend order to calls-then-cost; otherwise echarts sorts by
      // series index which can drift if we add series later.
      data: [t('analytics.callsColumn'), t('analytics.costColumn')],
    },
    grid: { left: 8, right: 8, top: 16, bottom: 32, containLabel: true },
    xAxis: {
      type: 'category',
      data: dates,
      axisTick: { alignWithLabel: true },
      axisLine: { lineStyle: { color: AXIS_LINE } },
      axisLabel: { fontSize: 11, color: TEXT_MUTED, hideOverlap: true },
    },
    // Two Y axes — index 0 (calls) on the left, index 1 (cost, yuan) on
    // the right. Each series binds itself to an axis via `yAxisIndex`.
    yAxis: [
      {
        type: 'value',
        name: t('analytics.callsColumn'),
        nameTextStyle: { color: TEXT_MUTED, fontSize: 11 },
        axisLabel: { fontSize: 11, color: TEXT_MUTED },
        splitLine: { lineStyle: { color: GRID_LINE } },
      },
      {
        type: 'value',
        name: t('analytics.costColumn'),
        nameTextStyle: { color: TEXT_MUTED, fontSize: 11 },
        axisLabel: {
          fontSize: 11,
          color: TEXT_MUTED,
          formatter: (v: number) => `¥${v}`,
        },
        splitLine: { show: false },
      },
    ],
    series: [
      {
        name: t('analytics.callsColumn'),
        type: 'bar',
        yAxisIndex: 0,
        data: calls,
        barMaxWidth: 36,
        itemStyle: { color: ACCENT, borderRadius: [4, 4, 0, 0] },
        emphasis: { itemStyle: { color: ACCENT } },
      },
      {
        name: t('analytics.costColumn'),
        type: 'line',
        yAxisIndex: 1,
        data: costs,
        smooth: true,
        showSymbol: true,
        symbolSize: 6,
        lineStyle: { color: PURPLE, width: 2 },
        itemStyle: { color: PURPLE },
      },
    ],
  }
})
</script>

<style scoped>
.trend-chart {
  width: 100%;
}

/* 240px tall chart matches the dashboard's section-card rhythm — tall
   enough for two axes + legend, short enough not to dominate the page. */
.trend-vchart {
  width: 100%;
  height: 240px;
}

.trend-empty {
  display: flex;
  align-items: center;
  justify-content: center;
  height: 240px;
  color: var(--color-text-muted);
  font-size: var(--text-sm);
}
</style>
