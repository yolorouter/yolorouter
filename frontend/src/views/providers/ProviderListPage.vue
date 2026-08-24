<!-- frontend/src/views/providers/ProviderListPage.vue -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('providers.eyebrow')" :title="t('providers.pageTitle')" :description="t('providers.pageDescription')">
      <template #actions>
        <n-button type="primary" @click="showCreate = true">
          <template #icon><Plus :size="16" /></template>
          {{ t('providers.createButton') }}
        </n-button>
      </template>
    </PageHeader>

    <EmptyState v-if="!store.loading && store.list.length === 0" :icon="Server" :title="t('providers.listEmpty')">
      <template #action>
        <n-button type="primary" @click="showCreate = true">{{ t('providers.createButton') }}</n-button>
      </template>
    </EmptyState>

    <template v-else>
      <div class="filter-panel">
        <div class="filter-grid">
          <div class="filter-item filter-item--search">
            <n-input
              v-model:value="filter.name"
              :placeholder="t('providers.filterName')"
              clearable
              size="small"
              @keyup.enter="onSearch"
            >
              <template #prefix><Search :size="14" /></template>
            </n-input>
          </div>
          <FilterSelectField
            v-model:value="filter.protocol"
            :label="t('providers.filterProtocol')"
            :options="protocolOptions"
            :placeholder="t('providers.filterProtocol')"
            size="small"
            width="100%"
            @update:value="onSearch"
          />
          <FilterSelectField
            v-model:value="filter.running"
            :label="t('providers.filterRunningStatus')"
            :options="runningStatusOptions"
            :placeholder="t('providers.filterRunningStatus')"
            size="small"
            width="100%"
            @update:value="onSearch"
          />
          <FilterSelectField
            v-model:value="filter.management"
            :label="t('providers.filterManagementStatus')"
            :options="managementStatusOptions"
            :placeholder="t('providers.filterManagementStatus')"
            size="small"
            width="100%"
            @update:value="onSearch"
          />
          <div class="filter-actions">
            <n-button size="small" type="primary" @click="onSearch">{{ t('providers.search') }}</n-button>
            <n-button size="small" quaternary @click="onReset">{{ t('providers.reset') }}</n-button>
          </div>
        </div>
      </div>

      <div class="data-table-wrapper">
        <ResponsiveDataTable
          :columns="columns"
          :data="filteredProviders"
          :loading="store.loading"
          :scroll-x="1010"
          :row-key="(row: Provider) => row.id"
          :row-props="rowProps"
          :full-span-keys="['expand_mappings']"
          :pagination="pagination"
        />
      </div>
    </template>

    <!-- store.create() (inside the modal) refetches the list itself; @created
         chains the first-setup flow straight into model import on the new
         provider's detail page. -->
    <NewProviderModal v-model:show="showCreate" @created="onProviderCreated" />
    <ProviderEditModal v-model:show="showEditProvider" :provider="editingProvider" @updated="onEdited" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { NButton, NSwitch, NTag, useDialog, useMessage, type DataTableColumns } from 'naive-ui'
import { MoreHorizontal, Plus, Search, Server } from '@lucide/vue'
import { useProvidersStore } from '../../store/providers'
import { useModelsStore } from '../../store/models'
import { useIsMobile } from '../../composables/useIsMobile'
import { displayMessage } from '../../api/client'
import type { Provider } from '../../api/providers'
import { useConfirmedStatusToggle } from '../../composables/useConfirmedStatusToggle'
import { providerDisableCopy } from '../../utils/impactSummary'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import NewProviderModal from '../../components/providers/NewProviderModal.vue'
import ProviderEditModal from '../../components/providers/ProviderEditModal.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import ResponsiveDropdown from '../../components/common/ResponsiveDropdown.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import { columnTitle } from '../../utils/columnTitle'
import { useClientPagination } from '../../composables/useClientPagination'
import { ALL_PROTOCOLS, enabledProtocolEndpoints } from '../../utils/providerProtocol'
import { PROVIDER_RUNNING_STATUS_DISPLAY, providerRunningStatusDisplay } from '../../utils/providerStatusDisplay'
import { expandPanel, EXPAND_EMPTY_STYLE, rowNavigationProps } from '../../utils/expandPanel'
import { routableMark } from '../../utils/modelStatusDisplay'
import type { VNodeChild } from 'vue'

const { t, te } = useI18n()
const router = useRouter()
const dialog = useDialog()
const toggleStatusWithConfirm = useConfirmedStatusToggle(dialog)
const message = useMessage()
const store = useProvidersStore()
const modelsStore = useModelsStore()
const isMobile = useIsMobile()
const showCreate = ref(false)

// First-setup handoff: a freshly created provider goes straight to its detail
// page with the import dialog auto-opened, so the admin lands in "pick your
// models" without hunting for the button. The flag travels through the store
// (consumed once by the detail page), not the URL — see the field's comment.
function onProviderCreated(created: Provider) {
  store.pendingImportProviderId = created.id
  void router.push(`/providers/${created.id}`)
}
// Inline row edit: open the provider edit modal straight from the list so a
// quick change needs no navigation into the detail page.
const showEditProvider = ref(false)
const editingProvider = ref<Provider | null>(null)

function openEditProvider(row: Provider) {
  editingProvider.value = row
  showEditProvider.value = true
}

// The expand panels derive their mapping rows and ✓/✗ marks from the models
// list, so any provider mutation that can change routability (status toggle,
// URL/protocol edit) must refetch models too — otherwise the mount-time
// snapshot keeps showing marks the mutation just invalidated.
function refreshModels() {
  void modelsStore.fetchList().catch((err) => message.error(displayMessage(err, t)))
}

function onEdited() {
  void store.fetchList().catch((err) => message.error(displayMessage(err, t)))
  refreshModels()
}

// In-page filters over the fully-fetched list. `filter` is the live draft the
// inputs edit; `applied` is the snapshot the table filters by — so the text
// field only takes effect on Enter / the Search button, mirroring the
// request-logs page.
interface ProviderFilter {
  name: string
  protocol: string | null
  running: string | null
  management: number | null
}
const emptyFilter = (): ProviderFilter => ({ name: '', protocol: null, running: null, management: null })
const filter = reactive<ProviderFilter>(emptyFilter())
const applied = reactive<ProviderFilter>(emptyFilter())

const protocolOptions = computed(() =>
  ALL_PROTOCOLS.map((p) => ({ label: t(`providers.protocol_${p}`), value: p })),
)
const runningStatusOptions = computed(() =>
  Object.entries(PROVIDER_RUNNING_STATUS_DISPLAY).map(([value, { i18nKey }]) => ({
    label: t(`providers.running${i18nKey}`),
    value,
  })),
)
const managementStatusOptions = computed(() => [
  { label: t('providers.statusEnabled'), value: 1 },
  { label: t('providers.statusDisabled'), value: 2 },
])

const filteredProviders = computed(() => {
  const q = applied.name.trim().toLowerCase()
  return store.list.filter((p) => {
    if (q && !p.name.toLowerCase().includes(q)) return false
    if (applied.protocol && p.provider_type !== applied.protocol) return false
    if (applied.running && p.running_status !== applied.running) return false
    if (applied.management !== null && p.management_status !== applied.management) return false
    return true
  })
})

// Client-side pagination: providers are few (admin-configured), so the full
// list is fetched once and sliced in the table rather than adding a
// server-side paged endpoint.
const { pagination } = useClientPagination()

// Applying a narrowed filter can leave the current page past the end of the
// results, so reset to the first page.
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
  void store.fetchList().catch((err) => message.error(displayMessage(err, t)))
  // Models feed the per-row mapping summary in the expand panel. A failure
  // only degrades that panel (it reads as "no models"), so it reports the
  // same way as the main fetch but doesn't block the list.
  void modelsStore.fetchList().catch((err) => message.error(displayMessage(err, t)))
})

function goDetail(id: number) {
  router.push(`/providers/${id}`)
}

function rowProps(row: Provider) {
  return rowNavigationProps(() => goDetail(row.id))
}

// The expand panel's mapping summary: every model with a candidate on this
// provider, as an aligned grid of "external name → provider-side name" plus
// the same ✓/✗ routability mark the model pages use. A same-name mapping
// says so instead of printing the name twice; a blank provider-side name
// shows a muted placeholder — the backend refuses to route such a candidate
// (its ✗ names the reason), so inventing a fallback name would dress up a
// dead route as a live one. Service addresses are already a permanent
// column, so the panel only adds what the row can't show. Inline styles
// because scoped CSS does not reach h()-rendered nodes.
function renderProviderExpand(row: Provider) {
  const cells: VNodeChild[] = []
  for (const m of modelsStore.list) {
    for (const c of m.candidates) {
      if (c.provider_id !== row.id) continue
      cells.push(
        h('span', { style: 'font-size:var(--text-sm); font-weight:500; color:var(--color-text);' }, m.name),
        h('span', { style: 'color:var(--color-text-muted);' }, '→'),
        !c.provider_model_name
          ? h('span', { style: 'font-size:var(--text-xs); color:var(--color-text-muted);' }, '—')
          : c.provider_model_name === m.name
            ? h('span', { style: 'font-size:var(--text-xs); color:var(--color-text-muted);' }, t('providers.expandSameName'))
            : h(
                'span',
                { style: 'font-family:var(--font-mono); font-size:var(--text-xs); color:var(--color-text-secondary);' },
                c.provider_model_name,
              ),
        routableMark(t, te, c, { inlineReason: isMobile.value }),
      )
    }
  }
  const eyebrow = isMobile.value ? undefined : t('providers.expandMappings')
  // "No mappings" is a claim about the data, so it must wait for the data:
  // while the models request is still in flight (or after it failed) an empty
  // list proves nothing, and mobile cards would assert the false state on
  // every row automatically.
  if (!modelsStore.list.length && modelsStore.loading) {
    return expandPanel({ eyebrow, indent: !isMobile.value }, [
      h('div', { style: EXPAND_EMPTY_STYLE }, t('common.loading')),
    ])
  }
  if (!modelsStore.list.length && modelsStore.error) {
    return expandPanel({ eyebrow, indent: !isMobile.value }, [
      h('div', { style: EXPAND_EMPTY_STYLE }, t('providers.expandMappingsLoadFailed')),
    ])
  }
  if (!cells.length) {
    return expandPanel({ eyebrow, indent: !isMobile.value }, [
      h('div', { style: EXPAND_EMPTY_STYLE }, t('providers.expandNoMappings')),
    ])
  }
  const grid = h(
    'div',
    {
      style:
        'display:grid; grid-template-columns:max-content max-content max-content max-content;' +
        ' column-gap:12px; row-gap:6px; align-items:center; justify-items:start;',
    },
    cells,
  )
  return expandPanel({ eyebrow, indent: !isMobile.value }, [grid])
}

// Every distinct address this provider actually serves on: the primary
// base_url plus each enabled additional-protocol endpoint (an endpoint with an
// empty URL reuses base_url, so it collapses into the same entry). Deduped so
// a provider whose extra protocols all reuse base_url still shows one address.
function serviceAddresses(row: Provider): string[] {
  const urls = [row.base_url]
  for (const { url } of enabledProtocolEndpoints(row.provider_type, row.protocol_endpoints)) {
    urls.push(url || row.base_url)
  }
  return [...new Set(urls)]
}

// Mirrors ProviderDetailPage.vue's onToggleProviderStatus, scoped to a list
// row instead of the single loaded detail — disabling still confirms first,
// enabling proceeds directly.
function onToggleStatus(row: Provider, enable: boolean) {
  const proceed = async () => {
    try {
      await store.setStatus(row.id, enable)
      await store.fetchList()
      refreshModels()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  }
  toggleStatusWithConfirm(
    enable,
    () => providerDisableCopy(row.id, t),
    proceed,
  )
}

// Desktop gets a real expand column; mobile card mode uses columns[0] as the
// card header, so the expand column must not lead there — the same mapping
// summary becomes a labeled card row instead.
const columns = computed<DataTableColumns<Provider>>(() =>
  isMobile.value
    ? [
        ...sharedColumns.value,
        {
          title: columnTitle(t('providers.expandMappings'), t('providers.expandMappings_tip')),
          key: 'expand_mappings',
          render: renderProviderExpand,
        },
      ]
    : [{ type: 'expand', renderExpand: renderProviderExpand }, ...sharedColumns.value],
)

// computed, not a plain const: column titles captured once at setup time
// would never re-translate after a locale switch (unlike each cell's own
// render(), which re-evaluates t() every render) — the sibling
// ProviderDetailPage.vue's keyColumns already gets this right via computed().
const sharedColumns = computed<DataTableColumns<Provider>>(() => [
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
    render: (row) =>
      h(
        'div',
        { class: 'provider-url-cell' },
        serviceAddresses(row).map((url) => h('div', { key: url, class: 'provider-url-line' }, url)),
      ),
  },
  {
    title: columnTitle(t('providers.protocolPrimary'), t('providers.protocolPrimary_tip')),
    key: 'provider_type',
    width: 150,
    render: (row) =>
      h(NTag, { size: 'small', bordered: false, round: true }, { default: () => t(`providers.protocol_${row.provider_type}`) }),
  },
  {
    title: columnTitle(t('providers.runningStatusColumn'), t('providers.runningStatusColumn_tip')),
    key: 'running_status',
    width: 210,
    render: (row) => {
      const display = providerRunningStatusDisplay(row.running_status)
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
              height: 150,
              options: [
                { label: t('providers.editProvider'), key: 'edit' },
                { label: t('costs.detail.viewCost'), key: 'viewCost' },
              ],
              onSelect: (key: string) => {
                if (key === 'edit') openEditProvider(row)
                else if (key === 'viewCost') router.push(`/costs/providers/${row.id}`)
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
:deep(.provider-name-cell) {
  font-weight: 650;
  color: var(--color-text);
}

:deep(.provider-url-cell) {
  display: flex;
  flex-direction: column;
  gap: 4px;
  color: var(--color-text-muted);
  font-size: var(--text-xs);
  font-family: var(--font-mono);
}

:deep(.provider-url-line) {
  word-break: break-all;
}
</style>
