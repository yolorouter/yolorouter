<!-- frontend/src/views/ratelimits/RateLimitListPage.vue
     The learned rate limits: what upstream 429s have said about each
     provider key's own ceilings. Read-mostly — the single action is the
     reset-delete, for when an upstream's real ceiling has moved past what
     only-down will ever record. Rows are bounded by keys × 2 meters, so
     one flat client-side-filtered list, no pagination. -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('rateLimits.eyebrow')" :title="t('rateLimits.pageTitle')" :description="t('rateLimits.pageDescription')" />

    <div class="filter-panel">
      <div class="filter-grid">
        <div class="filter-item filter-item--search">
          <n-input
            v-model:value="query"
            :placeholder="t('rateLimits.searchPlaceholder')"
            clearable
            size="small"
          >
            <template #prefix><Search :size="14" /></template>
          </n-input>
        </div>
      </div>
    </div>

    <div class="data-table-wrapper">
      <ResponsiveDataTable
        :columns="columns"
        :data="filteredRows"
        :loading="loading"
        :scroll-x="980"
        :row-key="(row: ObservedRateLimitRow) => row.id"
      >
        <template #empty>
          <EmptyState :icon="Gauge" :title="t('rateLimits.listEmpty')" />
        </template>
      </ResponsiveDataTable>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NTag, useDialog, useMessage, type DataTableColumns } from 'naive-ui'
import { Gauge, Search } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import { columnTitle } from '../../utils/columnTitle'
import { displayMessage } from '../../api/client'
import { getRateLimits, deleteRateLimit, type ObservedRateLimitRow } from '../../api/rateLimits'

const { t } = useI18n()
const message = useMessage()
const dialog = useDialog()

const rows = ref<ObservedRateLimitRow[]>([])
const loading = ref(false)
const query = ref('')

const filteredRows = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return rows.value
  return rows.value.filter(
    (r) =>
      r.provider_name.toLowerCase().includes(q) ||
      r.key_label.toLowerCase().includes(q) ||
      r.meter.includes(q),
  )
})

function formatNumberOrDash(v: number | null): string {
  return v === null ? '—' : v.toLocaleString()
}

function formatWindow(seconds: number | null): string {
  if (seconds === null) return '—'
  if (seconds % 86400 === 0) return t('rateLimits.windowDay', { n: seconds / 86400 })
  if (seconds % 3600 === 0) return t('rateLimits.windowHour', { n: seconds / 3600 })
  if (seconds % 60 === 0) return t('rateLimits.windowMinute', { n: seconds / 60 })
  return t('rateLimits.windowSecond', { n: seconds })
}

function formatTime(iso: string | null): string {
  if (!iso) return '—'
  const at = new Date(iso)
  return Number.isNaN(at.getTime()) ? '—' : at.toLocaleString()
}

function confirmReset(row: ObservedRateLimitRow) {
  dialog.warning({
    title: t('rateLimits.resetConfirmTitle'),
    content: t('rateLimits.resetConfirmContent'),
    positiveText: t('common.confirm'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await deleteRateLimit(row.id)
        message.success(t('rateLimits.resetSuccess'))
        await reload()
      } catch (err) {
        message.error(displayMessage(err, t))
      }
    },
  })
}

const columns = computed<DataTableColumns<ObservedRateLimitRow>>(() => [
  {
    title: columnTitle(t('rateLimits.providerColumn'), t('rateLimits.providerColumn_tip')),
    key: 'provider_name',
    minWidth: 160,
    render: (row) => row.provider_name || '—',
  },
  {
    title: columnTitle(t('rateLimits.keyColumn'), t('rateLimits.keyColumn_tip')),
    key: 'key_label',
    minWidth: 120,
    render: (row) => row.key_label || `#${row.provider_key_id}`,
  },
  {
    title: columnTitle(t('rateLimits.meterColumn'), t('rateLimits.meterColumn_tip')),
    key: 'meter',
    width: 110,
    render: (row) =>
      h(NTag, { size: 'small', bordered: false }, { default: () => row.meter === 'tokens' ? t('rateLimits.meterTokens') : t('rateLimits.meterRequests') }),
  },
  {
    title: columnTitle(t('rateLimits.limitColumn'), t('rateLimits.limitColumn_tip')),
    key: 'limit_value',
    width: 120,
    render: (row) => formatNumberOrDash(row.limit_value),
  },
  {
    title: columnTitle(t('rateLimits.windowColumn'), t('rateLimits.windowColumn_tip')),
    key: 'window_seconds',
    width: 120,
    render: (row) => formatWindow(row.window_seconds),
  },
  {
    title: columnTitle(t('rateLimits.remainingColumn'), t('rateLimits.remainingColumn_tip')),
    key: 'last_remaining',
    width: 120,
    render: (row) => formatNumberOrDash(row.last_remaining),
  },
  {
    title: columnTitle(t('rateLimits.resetColumn'), t('rateLimits.resetColumn_tip')),
    key: 'last_reset_at',
    minWidth: 170,
    render: (row) => formatTime(row.last_reset_at),
  },
  {
    title: columnTitle(t('rateLimits.updatedColumn'), t('rateLimits.updatedColumn_tip')),
    key: 'updated_at',
    minWidth: 170,
    render: (row) => formatTime(row.updated_at),
  },
  {
    title: t('rateLimits.actionsColumn'),
    key: 'actions',
    width: 90,
    render: (row) =>
      h(
        NButton,
        { size: 'small', quaternary: true, type: 'error', onClick: () => confirmReset(row) },
        { default: () => t('rateLimits.reset') },
      ),
  },
])

async function reload() {
  loading.value = true
  try {
    const page = await getRateLimits()
    rows.value = page.list
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void reload()
})
</script>
