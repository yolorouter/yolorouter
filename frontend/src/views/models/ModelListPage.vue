<!-- frontend/src/views/models/ModelListPage.vue -->
<template>
  <div class="models-page">
    <PageHeader :eyebrow="t('models.eyebrow')" :title="t('models.pageTitle')" :description="t('models.pageDescription')">
      <template #actions>
        <n-button type="primary" @click="showCreate = true">
          <template #icon><Plus :size="16" /></template>
          {{ t('models.createButton') }}
        </n-button>
      </template>
    </PageHeader>

    <EmptyState v-if="!store.loading && store.list.length === 0" :title="t('models.listEmpty')">
      <template #action>
        <n-button type="primary" @click="showCreate = true">{{ t('models.createButton') }}</n-button>
      </template>
    </EmptyState>

    <div v-else class="data-table-wrapper">
      <n-data-table
        :columns="columns"
        :data="store.list"
        :loading="store.loading"
        :bordered="false"
        :single-line="false"
        :row-key="(row: Model) => row.id"
        :row-props="rowProps"
      />
    </div>

    <NewModelDrawer v-model:show="showCreate" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { NSwitch, NTag, useDialog, useMessage, type DataTableColumns } from 'naive-ui'
import { Plus } from 'lucide-vue-next'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { toggleStatusWithConfirm } from '../../composables/useConfirmedStatusToggle'
import { modelRunningStatusDisplay } from '../../utils/modelStatusDisplay'
import type { Model } from '../../api/models'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import NewModelDrawer from '../../components/models/NewModelDrawer.vue'

const { t } = useI18n()
const router = useRouter()
const dialog = useDialog()
const message = useMessage()
const store = useModelsStore()
const showCreate = ref(false)

onMounted(() => store.fetchList())

function goDetail(id: number) {
  router.push(`/models/${id}`)
}

function rowProps(row: Model) {
  return { style: 'cursor: pointer', onClick: () => goDetail(row.id) }
}

function onToggleStatus(row: Model, enable: boolean) {
  toggleStatusWithConfirm(
    dialog,
    enable,
    {
      title: t('models.confirmDisableModelTitle'),
      content: t('models.confirmDisableModelContent', { count: 0 }),
      positiveText: t('models.statusDisabled'),
      negativeText: t('models.cancel'),
    },
    async () => {
      try {
        await store.setStatus(row.id, enable)
        await store.fetchList()
      } catch (err) {
        message.error(displayMessage(err, t))
      }
    },
  )
}

const columns = computed<DataTableColumns<Model>>(() => [
  {
    title: t('models.name'),
    key: 'name',
    minWidth: 200,
    render: (row) => h('span', { class: 'model-name-cell' }, row.name),
  },
  {
    title: t('models.runningStatusColumn'),
    key: 'running_status',
    width: 120,
    render: (row) => {
      const display = modelRunningStatusDisplay(row.running_status)
      return h(NTag, { size: 'small', bordered: false, type: display.tagType }, { default: () => t(`models.running${display.i18nKey}`) })
    },
  },
  {
    title: t('models.candidateCountColumn'),
    key: 'candidates',
    width: 140,
    render: (row) => `${row.candidates.filter((c) => c.routable).length} / ${row.candidates.length}`,
  },
  {
    title: t('models.firstRouteColumn'),
    key: 'first_route',
    minWidth: 200,
    render: (row) => {
      const first = row.candidates[0]
      return first ? `${first.provider_name} / ${first.provider_model_name}` : '-'
    },
  },
  {
    title: t('models.managementStatusColumn'),
    key: 'management_status',
    width: 100,
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
.models-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

:deep(.model-name-cell) {
  font-weight: 650;
  color: var(--color-text);
}
</style>
