<!-- frontend/src/views/models/ModelListPage.vue -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('models.eyebrow')" :title="t('models.pageTitle')" :description="t('models.pageDescription')">
      <template #actions>
        <n-button @click="showVisionFallback = true">
          {{ t('models.visionFallbackButton') }}
        </n-button>
        <n-button type="primary" @click="showCreate = true">
          <template #icon><Plus :size="16" /></template>
          {{ t('models.createButton') }}
        </n-button>
      </template>
    </PageHeader>

    <EmptyState v-if="!store.loading && store.list.length === 0" :icon="Boxes" :title="t('models.listEmpty')">
      <template #action>
        <n-button type="primary" @click="showCreate = true">{{ t('models.createButton') }}</n-button>
      </template>
    </EmptyState>

    <template v-else>
      <div class="filter-panel">
        <div class="filter-grid">
          <div class="filter-item filter-item--search">
            <n-input
              v-model:value="filter.name"
              :placeholder="t('models.filterName')"
              clearable
              size="small"
              @keyup.enter="onSearch"
            >
              <template #prefix><Search :size="14" /></template>
            </n-input>
          </div>
          <FilterSelectField
            v-model:value="filter.running"
            :label="t('models.filterRunningStatus')"
            :options="runningStatusOptions"
            :placeholder="t('models.filterRunningStatus')"
            size="small"
            width="100%"
            @update:value="onSearch"
          />
          <FilterSelectField
            v-model:value="filter.management"
            :label="t('models.filterManagementStatus')"
            :options="managementStatusOptions"
            :placeholder="t('models.filterManagementStatus')"
            size="small"
            width="100%"
            @update:value="onSearch"
          />
          <FilterSelectField
            v-model:value="filter.provider"
            :label="t('models.filterProvider')"
            :options="providerOptions"
            :placeholder="t('models.filterProvider')"
            filterable
            size="small"
            width="100%"
            @update:value="onSearch"
          />
          <div class="filter-actions">
            <n-button size="small" type="primary" @click="onSearch">{{ t('models.search') }}</n-button>
            <n-button size="small" quaternary @click="onReset">{{ t('models.reset') }}</n-button>
          </div>
        </div>
      </div>

      <div class="data-table-wrapper">
        <ResponsiveDataTable
          :columns="columns"
          :data="filteredModels"
          :loading="store.loading"
          :scroll-x="910"
          :row-key="(row: Model) => row.id"
          :row-props="rowProps"
          :full-span-keys="['expand_candidates']"
          :pagination="pagination"
        />
      </div>
    </template>

    <NewModelModal v-model:show="showCreate" />
    <ModelEditModal v-model:show="showEditModel" :model="editingModel" @updated="onEdited" />
    <VisionFallbackModal v-model:show="showVisionFallback" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NSwitch, NTag, useDialog, useMessage, type DataTableColumns } from 'naive-ui'
import { Boxes, Plus, Search, MoreHorizontal } from '@lucide/vue'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { useConfirmedStatusToggle } from '../../composables/useConfirmedStatusToggle'
import { modelDisableCopy } from '../../utils/impactSummary'
import { modelRunningStatusDisplay, MODEL_RUNNING_STATUS_DISPLAY, routableMark } from '../../utils/modelStatusDisplay'
import { columnTitle } from '../../utils/columnTitle'
import { isBalancedModel } from '../../utils/schedulingMode'
import { modelCostDetailLocation } from '../../utils/modelCostLocation'
import { useClientPagination } from '../../composables/useClientPagination'
import { useIsMobile } from '../../composables/useIsMobile'
import { expandPanel, EXPAND_EMPTY_STYLE, rowNavigationProps } from '../../utils/expandPanel'
import type { VNodeChild } from 'vue'
import type { Model, ModelCandidate } from '../../api/models'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import NewModelModal from '../../components/models/NewModelModal.vue'
import ModelEditModal from '../../components/models/ModelEditModal.vue'
import VisionFallbackModal from '../../components/models/VisionFallbackModal.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import ResponsiveDropdown from '../../components/common/ResponsiveDropdown.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import { ccsProfileName } from '../../utils/format'
import { useCCSwitchImport } from '../../composables/useCCSwitchImport'

const { t, te } = useI18n()
const isMobile = useIsMobile()
const route = useRoute()
const router = useRouter()
const dialog = useDialog()
const toggleStatusWithConfirm = useConfirmedStatusToggle(dialog)
const message = useMessage()
const store = useModelsStore()
const { importToCCS } = useCCSwitchImport()
const showCreate = ref(false)
const showVisionFallback = ref(false)
// Inline row edit: reuse the same edit modal the detail page uses, opened
// straight from the list so a name change needs no navigation.
const showEditModel = ref(false)
const editingModel = ref<Model | null>(null)

function openEditModel(row: Model) {
  editingModel.value = row
  showEditModel.value = true
}

function onEdited() {
  void store.fetchList().catch((err) => message.error(displayMessage(err, t)))
}

// In-page filters over the fully-fetched list (name substring + running /
// management status). `filter` is the live draft the inputs edit; `applied`
// is the snapshot the table actually filters by — mirroring the
// search/reset flow on the request-logs page so the text field only takes
// effect on Enter / the Search button, not on every keystroke.
interface ModelFilter {
  name: string
  running: string | null
  management: number | null
  provider: number | null
}
const emptyFilter = (): ModelFilter => ({ name: '', running: null, management: null, provider: null })
const filter = reactive<ModelFilter>(emptyFilter())
const applied = reactive<ModelFilter>(emptyFilter())

const runningStatusOptions = computed(() =>
  Object.entries(MODEL_RUNNING_STATUS_DISPLAY).map(([value, { i18nKey }]) => ({
    label: t(`models.running${i18nKey}`),
    value,
  })),
)
const managementStatusOptions = computed(() => [
  { label: t('models.statusEnabled'), value: 1 },
  { label: t('models.statusDisabled'), value: 2 },
])

// The provider filter answers "which models route through this provider" —
// options are the union of providers across every model's candidates, so
// only providers that actually appear somewhere are offered.
const providerOptions = computed(() => {
  const byId = new Map<number, string>()
  for (const m of store.list) {
    for (const c of m.candidates) {
      if (!byId.has(c.provider_id)) byId.set(c.provider_id, c.provider_name)
    }
  }
  return [...byId.entries()]
    .map(([value, label]) => ({ label, value }))
    .sort((a, b) => a.label.localeCompare(b.label))
})

// A selected provider can drop out of the options when its last candidate
// goes away and the list refreshes. Left in place it becomes a ghost filter:
// a blank select over an empty table with nothing explaining why.
watch(providerOptions, (opts) => {
  const known = (v: number | null) => v === null || opts.some((o) => o.value === v)
  if (!known(filter.provider)) filter.provider = null
  if (!known(applied.provider)) applied.provider = null
})

const filteredModels = computed(() => {
  const q = applied.name.trim().toLowerCase()
  return store.list.filter((m) => {
    if (q && !m.name.toLowerCase().includes(q)) return false
    if (applied.running && m.running_status !== applied.running) return false
    if (applied.management !== null && m.management_status !== applied.management) return false
    if (applied.provider !== null && !m.candidates.some((c) => c.provider_id === applied.provider)) return false
    return true
  })
})

// Client-side pagination: models are few (admin-configured), so the full list
// is fetched once and sliced in the table rather than adding a server-side
// paged endpoint.
const { pagination } = useClientPagination()

// A narrowed filter can leave the current page past the end of the results —
// reset to the first page whenever a filter is applied.
function onSearch() {
  Object.assign(applied, filter)
  pagination.page = 1
}
function onReset() {
  Object.assign(filter, emptyFilter())
  Object.assign(applied, emptyFilter())
  pagination.page = 1
}

onMounted(() => {
  // Deep links: ?management=1|2 (the dashboard's unavailable-models figure
  // counts disabled models, so its drill-down filters this dimension) and
  // ?running=<running status>. Other filters have no inbound links today;
  // add their keys here when one appears.
  const management = route.query.management
  if (management === '1' || management === '2') {
    filter.management = Number(management)
    applied.management = Number(management)
  }
  const running = route.query.running
  if (typeof running === 'string' && Object.hasOwn(MODEL_RUNNING_STATUS_DISPLAY, running)) {
    filter.running = running
    applied.running = running
  }
  void store.fetchList().catch((err) => message.error(displayMessage(err, t)))
})

function goDetail(id: number) {
  router.push(`/models/${id}`)
}

function rowProps(row: Model) {
  return rowNavigationProps(() => goDetail(row.id))
}

// The route chain as an aligned grid: order marker, provider, the provider's
// own name for the model (monospace — it's a wire identifier, not prose),
// and whether the gateway can actually send traffic there. A candidate whose
// provider-side name is still blank shows a muted placeholder — the backend
// refuses to route it (its ✗ names the reason), so inventing a fallback name
// here would dress up a dead route as a live one. Inline styles because
// scoped CSS does not reach h()-rendered nodes.
function candidateCells(c: ModelCandidate): VNodeChild[] {
  return [
    h(
      'span',
      { style: 'font-size:var(--text-xs); color:var(--color-text-muted); font-variant-numeric:tabular-nums; justify-self:end;' },
      String(c.sort_order),
    ),
    h('span', { style: 'font-size:var(--text-sm); font-weight:500; color:var(--color-text);' }, c.provider_name),
    c.provider_model_name
      ? h(
          'span',
          { style: 'font-family:var(--font-mono); font-size:var(--text-xs); color:var(--color-text-secondary);' },
          c.provider_model_name,
        )
      : h('span', { style: 'font-size:var(--text-xs); color:var(--color-text-muted);' }, '—'),
    routableMark(t, te, c, { inlineReason: isMobile.value }),
  ]
}

function renderModelExpand(row: Model) {
  if (!row.candidates.length) {
    return expandPanel({ eyebrow: isMobile.value ? undefined : t('models.expandCandidates'), indent: !isMobile.value }, [
      h('div', { style: EXPAND_EMPTY_STYLE }, t('models.expandNoCandidates')),
    ])
  }
  const grid = h(
    'div',
    {
      style:
        'display:grid; grid-template-columns:max-content max-content max-content max-content;' +
        ' column-gap:12px; row-gap:6px; align-items:center; justify-items:start;',
    },
    row.candidates.flatMap(candidateCells),
  )
  return expandPanel({ eyebrow: isMobile.value ? undefined : t('models.expandCandidates'), indent: !isMobile.value }, [grid])
}

function onToggleStatus(row: Model, enable: boolean) {
  toggleStatusWithConfirm(
    enable,
    () => modelDisableCopy(row.id, t),
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

// Desktop gets a real expand column (route order at a glance without leaving
// the list); mobile card mode uses columns[0] as the card header, so the
// expand column must not lead there — the same panel becomes a labeled card
// row instead.
const columns = computed<DataTableColumns<Model>>(() =>
  isMobile.value
    ? [
        ...sharedColumns.value,
        {
          title: columnTitle(t('models.expandCandidates'), t('models.expandCandidates_tip')),
          key: 'expand_candidates',
          render: renderModelExpand,
        },
      ]
    : [{ type: 'expand', renderExpand: renderModelExpand }, ...sharedColumns.value],
)

const sharedColumns = computed<DataTableColumns<Model>>(() => [
  {
    title: columnTitle(t('models.name'), t('models.name_tip')),
    key: 'name',
    minWidth: 200,
    render: (row) => h('span', { class: 'model-name-cell' }, row.name),
  },
  {
    title: columnTitle(t('models.runningStatusColumn'), t('models.runningStatusColumn_tip')),
    key: 'running_status',
    width: 180,
    render: (row) => {
      const display = modelRunningStatusDisplay(row.running_status)
      return h(NTag, { size: 'small', bordered: false, type: display.tagType }, { default: () => t(`models.running${display.i18nKey}`) })
    },
  },
  {
    title: columnTitle(t('models.candidateCountColumn'), t('models.candidateCountColumn_tip')),
    key: 'candidates',
    width: 140,
    render: (row) => `${row.candidates.filter((c) => c.routable).length} / ${row.candidates.length}`,
  },
  {
    title: columnTitle(t('models.firstRouteColumn'), t('models.firstRouteColumn_tip')),
    key: 'first_route',
    minWidth: 200,
    render: (row) => {
      // A balanced model has no fixed primary: each caller key enters the
      // chain at its own bound provider, so the cell names the mode instead
      // of candidates[0].
      const balanced = isBalancedModel(row)
      const first = row.candidates[0]
      const head = balanced
        ? h(NTag, { size: 'small', bordered: false }, { default: () => t('models.firstRouteBalanced') })
        : first
          ? `${first.provider_name} / ${first.provider_model_name}`
          : '-'
      // With a provider filter active, a row can match through a candidate
      // that is not the one shown here — the very thing that makes the row
      // look wrong ("why did Zhipu surface a deepseek model?"). Name the
      // matching candidate so the connection stays visible in both modes.
      const pid = applied.provider
      const matched = pid === null ? undefined : row.candidates.find((c) => c.provider_id === pid)
      if (!matched || (!balanced && matched === first)) return head
      return h('div', [
        h('div', [head]),
        h(
          'div',
          { style: 'font-size: 12px; color: #999;' },
          t('models.filterMatchedVia', {
            route: `${matched.provider_name} / ${matched.provider_model_name}`,
            position: matched.sort_order,
          }),
        ),
      ])
    },
  },
  {
    title: columnTitle(t('models.managementStatusColumn'), t('models.managementStatusColumn_tip')),
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
  {
    title: t('common.actions'),
    key: 'actions',
    align: 'center',
    width: 90,
    render: (row) =>
      h(
        'div',
        { onClick: (e: MouseEvent) => e.stopPropagation() },
        [
          h(
            ResponsiveDropdown,
            {
              trigger: 'click',
              placement: 'bottom-end',
              triggerText: t('common.actions'),
              height: 200,
              options: [
                { label: t('models.editModel'), key: 'edit' },
                { label: t('costs.detail.viewCost'), key: 'viewCost' },
                { label: t('ccswitch.importAction'), key: 'importCCSImport' },
              ],
              onSelect: (key: string) => {
                if (key === 'edit') openEditModel(row)
                else if (key === 'viewCost') router.push(modelCostDetailLocation(row.name))
                else if (key === 'importCCSImport')
                  importToCCS({ name: ccsProfileName(row.name), model: row.name })
              },
            },
            {
              default: () =>
                h(
                  NButton,
                  { size: 'small', quaternary: true, circle: true },
                  { icon: () => h(MoreHorizontal, { size: 16 }) },
                ),
            },
          ),
        ],
      ),
  },
])
</script>

<style scoped>
:deep(.model-name-cell) {
  font-weight: 650;
  color: var(--color-text);
}
</style>
