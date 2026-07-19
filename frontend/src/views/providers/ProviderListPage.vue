<!-- frontend/src/views/providers/ProviderListPage.vue -->
<template>
  <div class="providers-page">
    <PageHeader :eyebrow="t('providers.eyebrow')" :title="t('providers.pageTitle')" :description="t('providers.pageDescription')">
      <template #actions>
        <n-button type="primary" @click="showCreate = true">
          <template #icon><Plus :size="16" /></template>
          {{ t('providers.createButton') }}
        </n-button>
      </template>
    </PageHeader>

    <EmptyState v-if="!store.loading && store.list.length === 0" :title="t('providers.listEmpty')">
      <template #action>
        <n-button type="primary" @click="showCreate = true">{{ t('providers.createButton') }}</n-button>
      </template>
    </EmptyState>

    <div v-else class="data-table-wrapper">
      <n-data-table
        :columns="columns"
        :data="store.list"
        :loading="store.loading"
        :bordered="false"
        :single-line="false"
        :row-key="(row: Provider) => row.id"
        :row-props="rowProps"
      />
    </div>

    <!-- No @created handler needed: store.create() (called inside the
         drawer) already refetches the list itself. -->
    <NewProviderDrawer v-model:show="showCreate" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { NSwitch, NTag, useDialog, useMessage, type DataTableColumns } from 'naive-ui'
import { Plus } from '@lucide/vue'
import { useProvidersStore } from '../../store/providers'
import { displayMessage } from '../../api/client'
import type { Provider } from '../../api/providers'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import NewProviderDrawer from '../../components/providers/NewProviderDrawer.vue'
import { columnTitle } from '../../utils/columnTitle'

const { t } = useI18n()
const router = useRouter()
const dialog = useDialog()
const message = useMessage()
const store = useProvidersStore()
const showCreate = ref(false)

onMounted(() => store.fetchList())

function goDetail(id: number) {
  router.push(`/providers/${id}`)
}

function rowProps(row: Provider) {
  return { style: 'cursor: pointer', onClick: () => goDetail(row.id) }
}

// Single lookup table keyed by the same 5 running-status values instead of
// a separate map + switch that were always consulted together for the
// same row (a /simplify simplification-review finding).
const RUNNING_STATUS_DISPLAY: Record<string, { i18nKey: string; type: 'default' | 'success' | 'warning' | 'error' }> = {
  not_configured: { i18nKey: 'NotConfigured', type: 'default' },
  pending_test: { i18nKey: 'Pending', type: 'default' },
  available: { i18nKey: 'Available', type: 'success' },
  partial: { i18nKey: 'Partial', type: 'warning' },
  unavailable: { i18nKey: 'Unavailable', type: 'error' },
}

function runningStatusDisplay(status: string) {
  return RUNNING_STATUS_DISPLAY[status] ?? RUNNING_STATUS_DISPLAY.unavailable
}

// Mirrors ProviderDetailPage.vue's onToggleProviderStatus, scoped to a list
// row instead of the single loaded detail — disabling still confirms first,
// enabling proceeds directly.
function onToggleStatus(row: Provider, enable: boolean) {
  const proceed = async () => {
    try {
      await store.setStatus(row.id, enable)
      await store.fetchList()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  }
  if (!enable) {
    dialog.warning({
      title: t('providers.confirmDisableProviderTitle'),
      content: t('providers.confirmDisableProviderContent'),
      positiveText: t('providers.statusDisabled'),
      negativeText: t('providers.cancel'),
      onPositiveClick: proceed,
    })
    return
  }
  void proceed()
}

// computed, not a plain const: a max-effort code-review round found this
// was captured once at setup time, so column TITLES (unlike each cell's
// own render(), which re-evaluates t() every render) never re-translated
// after a locale switch — the sibling ProviderDetailPage.vue's keyColumns
// already gets this right via computed().
const columns = computed<DataTableColumns<Provider>>(() => [
  {
    title: columnTitle(t('providers.name'), t('providers.name_tip')),
    key: 'name',
    minWidth: 200,
    render: (row) => h('span', { class: 'provider-name-cell' }, row.name),
  },
  {
    title: columnTitle(t('providers.baseUrl'), t('providers.baseUrl_tip')),
    key: 'base_url',
    minWidth: 240,
    render: (row) => h('span', { class: 'provider-url-cell' }, row.base_url),
  },
  {
    title: columnTitle(t('providers.runningStatusColumn'), t('providers.runningStatusColumn_tip')),
    key: 'running_status',
    width: 210,
    render: (row) => {
      const display = runningStatusDisplay(row.running_status)
      return h(
        NTag,
        { size: 'small', bordered: false, type: display.type },
        { default: () => t(`providers.running${display.i18nKey}`) },
      )
    },
  },
  {
    title: columnTitle(t('providers.managementStatusColumn'), t('providers.managementStatusColumn_tip')),
    key: 'management_status',
    width: 120,
    render: (row) =>
      h(
        'div',
        { onClick: (e: MouseEvent) => e.stopPropagation() },
        [
          h(NSwitch, {
            size: 'small',
            value: row.management_status === 1,
            'onUpdate:value': (v: boolean) => onToggleStatus(row, v),
          }),
        ],
      ),
  },
])
</script>

<style scoped>
.providers-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

:deep(.provider-name-cell) {
  font-weight: 650;
  color: var(--color-text);
}

:deep(.provider-url-cell) {
  color: var(--color-text-muted);
  font-size: var(--text-xs);
  font-family: var(--font-mono);
}
</style>
