<!-- frontend/src/components/agent-sessions/SessionWaterfallBar.vue
     One timeline row's duration bar for the tool-session detail view. The
     parent computes the session-wide time axis once (earliest created_at →
     latest created_at + duration) and passes it in; this component maps its
     row onto that axis LINEARLY:

       left  % = (row.created_at − session start) / session span
       width % = row.duration_ms / session span

     so bars align on the real time axis — gaps read as think-time between
     calls, long bars as slow steps. Hovering shows the precise start
     timestamp and the end-to-end duration (NTooltip on pointer devices; the
     same string rides on the track's aria-label/title so it stays available
     to touch, keyboard, and tests). -->
<template>
  <NTooltip trigger="hover" placement="top">
    <template #trigger>
      <div class="waterfall" :aria-label="tip" :title="tip">
        <div class="waterfall__bar" :class="`waterfall__bar--${row.status_class}`" :style="barStyle" />
      </div>
    </template>
    {{ tip }}
  </NTooltip>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { NTooltip } from 'naive-ui'
import type { RequestLogRow } from '../../api/requestLogs'
import { formatDuration, formatShortClock } from '../../utils/format'

const props = defineProps<{
  row: RequestLogRow
  /** Session axis start (epoch ms): the earliest row's created_at. */
  startMs: number
  /** Session axis span (ms): latest (created_at + duration) minus startMs. */
  spanMs: number
}>()

const { t } = useI18n()

// Linear mapping onto the shared axis. Guards: a non-positive span (single
// instant request, or malformed timestamps) collapses to a full-width bar at
// the left edge so the row still reads as "one call, no pacing"; offsets and
// widths are clamped into [0, 100] so one anomalous row cannot push its bar
// outside the track.
const leftPct = computed(() => {
  if (props.spanMs <= 0) return 0
  const offset = Date.parse(props.row.created_at) - props.startMs
  return clamp((offset / props.spanMs) * 100)
})
const widthPct = computed(() => {
  if (props.spanMs <= 0) return 100
  return clamp((props.row.duration_ms / props.spanMs) * 100)
})

function clamp(pct: number): number {
  if (Number.isNaN(pct)) return 0
  return Math.min(100, Math.max(0, pct))
}

const barStyle = computed(() => ({
  left: `${leftPct.value}%`,
  width: `${widthPct.value}%`,
}))

// The tooltip text: precise start timestamp plus the duration. Both pieces
// formatted exactly as the timeline's own cells format them (the shared
// utils/format formatters), so the hover confirms what the columns say at
// full precision.
const tip = computed(() =>
  t('agentSessions.waterfallTip', {
    time: formatShortClock(props.row.created_at),
    duration: formatDuration(props.row.duration_ms),
  }),
)
</script>

<style scoped>
/* The track fills the cell; the bar is absolutely positioned inside it.
   Height matches a compact table row (the column reserves 8px + padding). */
.waterfall {
  position: relative;
  width: 100%;
  min-width: 120px;
  height: 10px;
  border-radius: 5px;
  background: var(--color-bg-soft, #f2f3f5);
  overflow: hidden;
}

.waterfall__bar {
  position: absolute;
  top: 0;
  height: 100%;
  min-width: 2px; /* a near-zero duration stays visible as a sliver */
  border-radius: 5px;
}

/* Bar colour follows the row's five-class status, mirroring StatusClassTag:
   success green, failed red, partial/rejected amber, cancelled neutral. */
.waterfall__bar--success {
  background: var(--color-success);
}
.waterfall__bar--failed {
  background: var(--color-danger);
}
.waterfall__bar--partial,
.waterfall__bar--rejected {
  background: var(--color-warning);
}
.waterfall__bar--cancelled {
  background: var(--color-text-muted, #909399);
}
</style>
