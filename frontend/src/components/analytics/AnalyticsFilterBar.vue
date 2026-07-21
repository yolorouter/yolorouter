<!-- frontend/src/components/analytics/AnalyticsFilterBar.vue
     The AnalyticsPage filter bar. Wraps TimeRangeSelect
     + ApiKey / Model / Provider / Status selectors. Each selector fetches its
     own option list in onMounted and emits an analytics.AnalyticsFilter
     payload upstream whenever any selector changes.

     The component is a controlled input — the parent owns the canonical
     filter state. That keeps the dimension-tab/columns/tab metadata in one
     place (the page) and lets the page reload the report on any filter
     change without coordinating state across components. -->
<template>
  <div class="filter-bar">
    <div class="filter-field">
      <label class="filter-label">{{ t('analytics.timeRange') }}</label>
      <TimeRangeSelect
        :model-value="timeRange"
        :preset="preset"
        @update:model-value="onTimeRange"
        @update:preset="onPreset"
      />
    </div>

    <div class="filter-field">
      <label class="filter-label">{{ t('analytics.apiKey') }}</label>
      <NSelect
        :value="filter.api_key_id ?? null"
        :options="apiKeyOptions"
        size="small"
        clearable
        filterable
        :placeholder="t('analytics.allApiKey')"
        style="width: 200px"
        @update:value="(v: number | null) => update('api_key_id', v)"
      />
    </div>

    <div class="filter-field">
      <label class="filter-label">{{ t('analytics.model') }}</label>
      <NSelect
        :value="filter.model_name ?? null"
        :options="modelOptions"
        size="small"
        clearable
        filterable
        :placeholder="t('analytics.allModel')"
        style="width: 200px"
        @update:value="(v: string | null) => update('model_name', v)"
      />
    </div>

    <div class="filter-field">
      <label class="filter-label">{{ t('analytics.provider') }}</label>
      <NSelect
        :value="filter.provider_id ?? null"
        :options="providerOptions"
        size="small"
        clearable
        filterable
        :placeholder="t('analytics.allProvider')"
        style="width: 200px"
        @update:value="(v: number | null) => update('provider_id', v)"
      />
    </div>

    <div class="filter-field">
      <label class="filter-label">{{ t('analytics.status') }}</label>
      <NSelect
        :value="filter.status ?? ''"
        :options="statusOptions"
        size="small"
        clearable
        :placeholder="t('analytics.allStatus')"
        style="width: 160px"
        @update:value="(v: string | null) => update('status', v || null)"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NSelect, type SelectOption } from 'naive-ui'
import TimeRangeSelect, { type RangePreset, type TimeRange } from './TimeRangeSelect.vue'
import { listProviders } from '../../api/providers'
import { listModels } from '../../api/models'
import { listAPIKeys, toAPIKeyOptions } from '../../api/apiKeys'
import type { AnalyticsFilter } from '../../api/analytics'
import { displayMessage } from '../../api/client'
import { useMessage } from 'naive-ui'

const props = defineProps<{
  filter: AnalyticsFilter
  timeRange: TimeRange
  preset: RangePreset
}>()
const emit = defineEmits<{
  'update:filter': [value: AnalyticsFilter]
  'update:timeRange': [value: TimeRange]
  'update:preset': [value: RangePreset]
}>()

const { t } = useI18n()
const message = useMessage()

// Filter option lists — fetched once on mount. These are admin-configured
// catalogs (not request-derived), so the lists are small and change
// infrequently; refetching on every filter change would be wasteful.
const apiKeyOptions = ref<SelectOption[]>([])
const providerOptions = ref<SelectOption[]>([])
const modelOptions = ref<SelectOption[]>([])

const statusOptions = computed<SelectOption[]>(() => [
  { label: t('analytics.statusSuccess'), value: 'success' },
  { label: t('analytics.statusFailed'), value: 'failed' },
  { label: t('analytics.statusPartial'), value: 'partial' },
  { label: t('analytics.statusCancelled'), value: 'cancelled' },
  { label: t('analytics.statusRejected'), value: 'rejected' },
])

onMounted(async () => {
  try {
    // Fire all three fetches in parallel — they're independent and add up
    // to ~3 round trips, which serialized would block the filter bar for
    // longer than the user expects on first paint.
    const [providerPage, modelPage, apiKeyPage] = await Promise.all([
      listProviders(),
      listModels(),
      listAPIKeys('', 1, 200), // 200 is plenty for an admin's key catalog
    ])
    providerOptions.value = providerPage.list.map((p) => ({ label: p.name, value: p.id }))
    modelOptions.value = modelPage.list.map((m) => ({ label: m.name, value: m.name }))
    apiKeyOptions.value = toAPIKeyOptions(apiKeyPage.list)
  } catch (err) {
    // Filter selectors being empty is a degraded, not a broken, state — the
    // user can still type a model name manually if the catalog fetch fails.
    // Show the error inline but don't block the page.
    message.error(displayMessage(err, t))
  }
})

function update<K extends keyof AnalyticsFilter>(key: K, value: AnalyticsFilter[K]) {
  emit('update:filter', { ...props.filter, [key]: value })
}

function onTimeRange(v: TimeRange) {
  emit('update:timeRange', v)
  emit('update:filter', {
    ...props.filter,
    start: v.start,
    end: v.end,
  })
}

function onPreset(v: RangePreset) {
  emit('update:preset', v)
}
</script>

<style scoped>
.filter-bar {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-4);
  padding: var(--space-4);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
}

.filter-field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.filter-label {
  font-size: var(--text-xs);
  font-weight: 700;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--color-text-muted);
}
</style>
