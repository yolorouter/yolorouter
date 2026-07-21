<!-- frontend/src/components/analytics/TimeRangeSelect.vue
     Time range picker shared between the AnalyticsPage filter bar and any
     future dashboard that wants the same presets. Emits a {start,end} tuple
     in RFC3339 form (server's /analytics endpoints expect RFC3339).

     Preset list: Today / Yesterday / Last 7d / Last 30d / Custom. "Custom"
     reveals an NDatePicker range panel; NDatePicker is not in main.ts's
     create() components list, so it's imported explicitly. -->
<template>
  <div class="time-range">
    <NSelect
      :value="preset"
      :options="presetOptions"
      size="small"
      style="width: 160px"
      @update:value="onPresetChange"
    />
    <NDatePicker
      v-if="preset === 'custom'"
      :value="customRange"
      type="daterange"
      size="small"
      clearable
      :placeholder="t('analytics.customRangePlaceholder')"
      @update:value="onCustomChange"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NDatePicker, NSelect, type SelectOption } from 'naive-ui'
import { useAuthStore } from '../../store/auth'

// RangePreset enumerates the named windows the picker offers. The string
// values are internal (not sent to the backend) — the backend receives
// resolved start/end timestamps only.
export type RangePreset = 'today' | 'yesterday' | 'last7d' | 'last30d' | 'custom'

export interface TimeRange {
  start: string | null // RFC3339, inclusive
  end: string | null // RFC3339, exclusive
}

const props = defineProps<{
  modelValue: TimeRange
  preset: RangePreset
}>()
const emit = defineEmits<{
  'update:modelValue': [value: TimeRange]
  'update:preset': [value: RangePreset]
}>()

const { t } = useI18n()
const authStore = useAuthStore()

// startOfTodayInZone returns the UTC instant of local-midnight "today" in a
// timezone given by its UTC offset (minutes east of UTC). When offset is null
// (server offset not yet known — e.g. before /auth/me resolves), it falls
// back to the browser's own timezone, preserving the pre-fix behavior for the
// dev case where server and browser share a zone. Offset-based computation
// avoids pulling a timezone library; DST is correct for "today" because the
// server's CURRENT offset is what's in effect right now (today is by the
// system's current timezone).
function startOfTodayInZone(offsetMinutes: number | null): Date {
  const offset = offsetMinutes ?? -new Date().getTimezoneOffset()
  const serverNowMs = Date.now() + offset * 60_000
  const serverNow = new Date(serverNowMs)
  // Server wall-clock midnight expressed as a UTC instant of that Y-M-D, then
  // shifted back by the offset to the true UTC time the server would store.
  const serverMidnightMs = Date.UTC(
    serverNow.getUTCFullYear(),
    serverNow.getUTCMonth(),
    serverNow.getUTCDate(),
    0,
    0,
    0,
    0,
  )
  return new Date(serverMidnightMs - offset * 60_000)
}

const presetOptions = computed<SelectOption[]>(() => [
  { label: t('analytics.rangeToday'), value: 'today' },
  { label: t('analytics.rangeYesterday'), value: 'yesterday' },
  { label: t('analytics.rangeLast7d'), value: 'last7d' },
  { label: t('analytics.rangeLast30d'), value: 'last30d' },
  { label: t('analytics.rangeCustom'), value: 'custom' },
])

// Internal custom-range state — kept as a tuple [startMs, endMs] because
// that's what NDatePicker daterange emits. We localize the boundaries to
// the user's timezone via toISOString() at emit time so the server's UTC
// storage gets compared correctly.
const customRange = ref<[number, number] | null>(null)

// resolvePreset returns the [start, end) window for a named preset, in the
// user's local timezone. end is exclusive (the start of tomorrow / the day
// after the range); start is inclusive (local midnight of the appropriate
// day). Mirrors the Go side's TodayBounds logic — both sides use local
// midnight so "today" means the same thing on both sides of the wire.
function resolvePreset(p: RangePreset): TimeRange {
  const startOfToday = startOfTodayInZone(authStore.serverTimezoneOffset)
  const endOfToday = new Date(startOfToday)
  endOfToday.setDate(endOfToday.getDate() + 1)
  switch (p) {
    case 'today':
      return { start: startOfToday.toISOString(), end: endOfToday.toISOString() }
    case 'yesterday': {
      const start = new Date(startOfToday)
      start.setDate(start.getDate() - 1)
      return { start: start.toISOString(), end: startOfToday.toISOString() }
    }
    case 'last7d': {
      const start = new Date(startOfToday)
      start.setDate(start.getDate() - 6) // 7 calendar days inclusive of today
      return { start: start.toISOString(), end: endOfToday.toISOString() }
    }
    case 'last30d': {
      const start = new Date(startOfToday)
      start.setDate(start.getDate() - 29) // 30 calendar days inclusive of today
      return { start: start.toISOString(), end: endOfToday.toISOString() }
    }
    default:
      // custom is handled by onCustomChange — no preset window here.
      return { start: null, end: null }
  }
}

function onPresetChange(v: RangePreset) {
  emit('update:preset', v)
  if (v === 'custom') {
    // Don't emit a new range — leave the existing customRange as the source
    // of truth. The user opens the picker and selects the next window.
    return
  }
  emit('update:modelValue', resolvePreset(v))
}

function onCustomChange(v: [number, number] | null) {
  customRange.value = v
  if (!v) {
    emit('update:modelValue', { start: null, end: null })
    return
  }
  const [startMs, endMs] = v
  // NDatePicker daterange emits [startMs, endMs] both at local midnight;
  // make end exclusive by adding 1 day so the server's [start, end) matches
  // the user's visible selection.
  const start = new Date(startMs)
  const end = new Date(endMs)
  end.setDate(end.getDate() + 1)
  emit('update:modelValue', { start: start.toISOString(), end: end.toISOString() })
}

// When the parent resets preset back to a named window (e.g. another tab's
// defaults load), re-derive the window. Without this, switching from
// "custom" back to "today" leaves the stale custom range on the wire.
watch(
  () => props.preset,
  (p) => {
    if (p !== 'custom') {
      emit('update:modelValue', resolvePreset(p))
    }
  },
  { immediate: true },
)
</script>

<style scoped>
.time-range {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
}
</style>
