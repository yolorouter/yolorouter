<!-- frontend/src/views/apikeys/ApiKeyListPage.vue -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('apiKeys.eyebrow')" :title="t('apiKeys.pageTitle')" :description="t('apiKeys.pageDescription')">
      <template #actions>
        <n-button type="primary" @click="showCreate = true">
          <template #icon><Plus :size="16" /></template>
          {{ t('apiKeys.createButton') }}
        </n-button>
      </template>
    </PageHeader>

    <!-- Gateway access info: the address API clients should point at, on
         the same screen as the keys themselves — credentials and endpoint
         only make sense together, and this page is where users land once
         configuration is done. -->
    <ApiAccessPanel />

    <div class="filter-panel">
      <div class="filter-grid">
        <div class="filter-item filter-item--search">
          <n-input
            v-model:value="draft.query"
            :placeholder="t('apiKeys.searchPlaceholder')"
            clearable
            size="small"
            @keyup.enter="onSearch"
          >
            <template #prefix><Search :size="14" /></template>
          </n-input>
        </div>
        <FilterSelectField
          v-if="authStore.isAdmin"
          v-model:value="draft.userId"
          :label="t('apiKeys.filterUser')"
          :options="userOptions"
          :placeholder="t('apiKeys.filterUser')"
          filterable
          size="small"
          width="100%"
          @update:value="onSearch"
        />
        <FilterSelectField
          v-model:value="draft.status"
          :label="t('apiKeys.filterStatus')"
          :options="statusOptions"
          :placeholder="t('apiKeys.filterStatus')"
          size="small"
          width="100%"
          @update:value="onSearch"
        />
        <div class="filter-actions">
          <n-button size="small" type="primary" @click="onSearch">{{ t('apiKeys.search') }}</n-button>
          <n-button size="small" quaternary @click="onReset">{{ t('apiKeys.reset') }}</n-button>
        </div>
      </div>
    </div>

    <EmptyState v-if="!store.loading && store.list.length === 0 && !store.total && !draftValueLength" :icon="KeyRound" :title="t('apiKeys.listEmpty')">
      <template #action>
        <n-button type="primary" @click="showCreate = true">{{ t('apiKeys.createButton') }}</n-button>
      </template>
    </EmptyState>

    <div v-else class="data-table-wrapper">
      <ResponsiveDataTable
        :columns="columns"
        :data="store.list"
        :loading="store.loading"
        :scroll-x="1040"
        :row-key="(row: APIKey) => row.id"
        :pagination="pagination"
        remote
      />
    </div>

    <CreateKeyModal v-model:show="showCreate" @created="onCreated" />
    <EditKeyModal v-if="editingId" :key="editingId" :show="showEdit" :api-key-id="editingId" @update:show="onEditShow" @saved="onSaved" />
    <KeyOptimize
      v-if="compressKeyId"
      :key="compressKeyId"
      :show="showCompress"
      :api-key-id="compressKeyId"
      @update:show="openOptimizeShow"
      @saved="openOptimizeSaved"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, onUnmounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { NButton, NInput, NTag, NTooltip, useDialog, useMessage, type DataTableColumns, type DropdownOption, type PaginationProps } from 'naive-ui'
import { KeyRound, Plus, Search, MoreHorizontal, Copy } from '@lucide/vue'
import { useApiKeysStore } from '../../store/apiKeys'
import { useAuthStore } from '../../store/auth'
import { displayMessage, errorCodeOf } from '../../api/client'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { formatMicros } from '../../utils/money'
import { ccsProfileName } from '../../utils/format'
import { useCCSwitchImport } from '../../composables/useCCSwitchImport'
import { useUserOptions } from '../../composables/useUserOptions'
import { copyToClipboard } from '../../utils/clipboard'
import ApiAccessPanel from '../../components/apikeys/ApiAccessPanel.vue'
import { listModels, type Model } from '../../api/models'
import { discoverGatewayModels, ERRCODE_KEY_PLAINTEXT_UNAVAILABLE, type APIKey } from '../../api/apiKeys'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import CreateKeyModal from '../../components/apikeys/CreateKeyModal.vue'
import EditKeyModal from '../../components/apikeys/EditKeyModal.vue'
import KeyOptimize from '../../components/apikeys/KeyOptimize.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import ResponsiveDropdown from '../../components/common/ResponsiveDropdown.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'

const { t } = useI18n()
const router = useRouter()
const dialog = useDialog()
const message = useMessage()
const store = useApiKeysStore()
const authStore = useAuthStore()
const { importToCCS } = useCCSwitchImport()
const showCreate = ref(false)

const showEdit = ref(false)
const editingId = ref<number | null>(null)
const showCompress = ref(false)
const compressKeyId = ref<number | null>(null)
const models = ref<Model[]>([])
const { userOptions, loadUserOptions } = useUserOptions()

// Live draft of the filter controls. The text inputs only apply on Enter or
// the Search button; the selects (status, owner account) apply immediately
// on change — matching the request-logs page.
const draft = reactive({
  query: store.query,
  status: (store.status || null) as string | null,
  userId: store.userId as number | null,
})

const statusOptions = computed(() => [
  { label: t('apiKeys.statusActive'), value: 'active' },
  { label: t('apiKeys.statusExpired'), value: 'expired' },
  { label: t('apiKeys.statusBudgetExhausted'), value: 'budget_exhausted' },
  { label: t('apiKeys.statusRevoked'), value: 'revoked' },
])

const draftValueLength = computed(() => {
  return Object.values(draft).filter(e => !!e).length
})

onMounted(() => {
  // The model catalog is admin-only; members don't render any model-derived
  // cell, so they only load their own key list.
  const loads = authStore.isAdmin ? [store.fetchList(), fetchModels(), loadUserOptions()] : [store.fetchList()]
  void Promise.all(loads).catch((err) => message.error(displayMessage(err, t)))
})

async function fetchModels() {
  const { list } = await listModels()
  models.value = list
}

async function reload() {
  try {
    await store.fetchList()
  } catch (err) {
    message.error(displayMessage(err, t))
  }
}

// setFilters resets the store to page 1, so a search always lands on the
// first page of results.
function onSearch() {
  store.setFilters({ query: draft.query.trim(), status: draft.status ?? '', userId: draft.userId })
  void reload()
}
function onReset() {
  draft.query = ''
  draft.status = null
  draft.userId = null
  store.setFilters({ query: '', status: '', userId: null })
  void reload()
}

const pagination = computed<PaginationProps>(() => ({
  page: store.page,
  pageSize: store.pageSize,
  itemCount: store.total,
  showSizePicker: true,
  pageSizes: [10, 20, 50, 100],
  onChange: (page: number) => {
    store.setPage(page)
    void reload()
  },
  onUpdatePageSize: (pageSize: number) => {
    store.setPageSize(pageSize)
    void reload()
  },
}))

function budgetCell(row: APIKey): string {
  const spent = formatMicros(row.budget_spent_micros)
  if (row.budget_limit_micros == null) return `${spent} / ${t('apiKeys.unlimited')}`
  return `${spent} / ${formatMicros(row.budget_limit_micros)}`
}

function expiresCell(row: APIKey): string {
  if (row.expires_at == null) return t('apiKeys.noExpiry')
  return new Date(row.expires_at).toLocaleString()
}

function statusTagType(s: string): 'success' | 'warning' | 'error' {
  if (s === 'active') return 'success'
  if (s === 'revoked') return 'error'
  return 'warning'
}

function statusLabel(s: string): string {
  if (s === 'active') return t('apiKeys.statusActive')
  if (s === 'expired') return t('apiKeys.statusExpired')
  if (s === 'budget_exhausted') return t('apiKeys.statusBudgetExhausted')
  return t('apiKeys.statusRevoked')
}

function openEdit(id: number) {
  editingId.value = id
  showEdit.value = true
}

// Cancel/X close the modal via update:show=false — clear editingId too so
// v-if="editingId" flips off and the next openEdit (same row or another)
// remounts the modal and re-runs onMounted/fill. Without this, reopening the
// same row would reuse the stale form from the previous open.
function onEditShow(v: boolean) {
  showEdit.value = v
  if (!v) editingId.value = null
}

function openOptimize(row: APIKey) {
  compressKeyId.value = row.id
  showCompress.value = true
}

function openOptimizeShow(v: boolean) {
  showCompress.value = v
  if (!v) compressKeyId.value = null
}

function openOptimizeSaved() {
  showCompress.value = false
  compressKeyId.value = null
  message.success(t('apiKeys.saveSuccess'))
  void reload()
}

function confirmRevoke(row: APIKey) {
  dialog.warning({
    title: t('apiKeys.confirmRevokeTitle'),
    content: t('apiKeys.confirmRevokeContent'),
    positiveText: t('apiKeys.revoke'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await store.revoke(row.id)
        message.success(t('apiKeys.revokeSuccess'))
        await reload()
      } catch (err) {
        message.error(displayMessage(err, t))
      }
    },
  })
}

// copyPlaintext fetches the full key from the reveal endpoint and writes it to
// the clipboard. The backend returns 11016 for keys that predate the
// encrypted_key column — displayMessage surfaces that localized message; other
// failures fall through to the generic error toast.
//
// One reveal at a time across the whole table, not one per row. Two in flight
// write to the same clipboard in whatever order they come back, so the key the
// user ends up holding is the slower request's, not the row they clicked last —
// and either request's finally would clear the other's loading state.
const revealingId = ref<number | null>(null)
async function copyPlaintext(row: APIKey) {
  if (revealingId.value !== null) return
  revealingId.value = row.id
  try {
    const res = await store.fetchPlaintext(row.id)
    // copyToClipboard handles the non-secure-context (plain HTTP) fallback to
    // execCommand internally; a false return means the write truly failed.
    if (await copyToClipboard(res.plaintext_key)) {
      message.success(t('common.copied'))
    } else {
      showPlaintextToCopyByHand(res.plaintext_key)
    }
  } catch (err) {
    // Fetch-side failure (incl. the legacy-key 11016) — distinct from a
    // clipboard-write failure, which is handled above.
    message.error(displayMessage(err, t))
  } finally {
    revealingId.value = null
  }
}

// showPlaintextToCopyByHand puts the key somewhere the user can select it.
//
// Every automatic path has already failed by the time this runs: the Clipboard
// API is unavailable or refused, and execCommand did not work either. Telling
// somebody to "select and copy manually" while the key exists only in a local
// variable leaves them with a button that does nothing and no way to try again
// — the reveal itself is repeatable, but they have no reason to think a second
// click behaves differently.
function showPlaintextToCopyByHand(plaintext: string) {
  dialog.warning({
    title: t('common.copyFailed'),
    content: () =>
      h(NInput, {
        value: plaintext,
        readonly: true,
        type: 'textarea',
        autosize: { minRows: 2, maxRows: 4 },
        onFocus: (e: FocusEvent) => (e.target as HTMLTextAreaElement | null)?.select(),
      }),
    positiveText: t('common.close'),
  })
}

function onCreated() {
  message.success(t('apiKeys.createSuccess'))
  void reload()
}

function onSaved() {
  showEdit.value = false
  // Reset editingId so the modal unmounts (v-if="editingId"); reopening it
  // remounts and re-runs onMounted/fill instead of showing stale form state.
  editingId.value = null
  message.success(t('apiKeys.saveSuccess'))
  void reload()
}

function firstUsableModel(row: APIKey): string | undefined {
  if (row.allow_all_models) return models.value.find((m) => m.running_status === 'available')?.name ?? models.value[0]?.name
  const firstAllowedModelId = row.model_ids[0]
  return models.value.find((model) => model.id === firstAllowedModelId)?.name
}

// importKeyToCCS hands the key to CC-Switch with its REAL plaintext and a
// model this key can actually route to. The plaintext comes from the
// re-view endpoint (owner-scoped, so members reach it for their own keys);
// the model comes from discoverGatewayModels — the gateway's own /v1/models
// AUTHED WITH THIS KEY, the one source that is correct for both roles
// (members cannot read the admin model catalog) and reflects the key's
// real scope. The admin catalog (firstUsableModel) is only a fallback when
// discovery fails.
//
// Reveal failures split by cause: only the permanent legacy case
// (plaintext never stored) imports with the placeholder plus a paste-by-
// hand toast. Any other failure is transient — importing the placeholder
// then would silently hand CC-Switch a wrong key for a perfectly readable
// credential — so the import aborts with the real error and the user
// retries. The legacy case cannot lose the model fallback either: plaintext
// storage predates per-account key ownership, so a legacy key can only
// belong to the admin-era account, and admin viewers have the catalog
// loaded for firstUsableModel.
//
// One import at a time (same single-flight rule as copyPlaintext, same
// reason): two in flight would race to the deep link, and the profile
// CC-Switch opens would be whichever request finished last, not the row the
// user clicked last. A completion that lands after the page unmounted must
// not fire the deep link from an unrelated page either — the unmounted
// flag drops it.
const importingId = ref<number | null>(null)
const unmounted = ref(false)
onUnmounted(() => {
  unmounted.value = true
})

// pickDiscoveredModel chooses among the key-scoped names /v1/models
// returned. The gateway lists management-enabled models only and says
// nothing about live availability, while the admin catalog knows
// running_status but not the key's scope — so intersect the two: the first
// discovered name the catalog marks available wins. Members have an empty
// catalog and keep the gateway's first entry.
function pickDiscoveredModel(names: string[]): string | undefined {
  const available = names.find((name) =>
    models.value.some((m) => m.name === name && m.running_status === 'available'))
  return available ?? names[0]
}

async function importKeyToCCS(row: APIKey) {
  if (importingId.value !== null) return
  importingId.value = row.id
  try {
    // Owner + key id, so several keys of one account import as
    // distinguishable CC-Switch profiles instead of identical names. The id
    // is unique per key; two distinct keys can share a truncated 16-char
    // prefix, so it would not.
    const identity = row.owner_username ? `${row.owner_username} (#${row.id})` : `#${row.id}`
    const name = ccsProfileName(identity)
    let plaintext: string | undefined
    try {
      plaintext = (await store.fetchPlaintext(row.id)).plaintext_key
    } catch (err) {
      if (errorCodeOf(err) !== ERRCODE_KEY_PLAINTEXT_UNAVAILABLE) {
        message.error(displayMessage(err, t))
        return
      }
      message.warning(t('ccswitch.plaintextUnavailable'))
    }
    let model: string | undefined
    if (plaintext) {
      try {
        model = pickDiscoveredModel(await discoverGatewayModels(plaintext))
      } catch {
        // Discovery is best-effort; fall through to the catalog fallback.
      }
    }
    if (unmounted.value) return
    importToCCS({
      name,
      apiKey: plaintext,
      model: model ?? firstUsableModel(row),
    })
  } finally {
    importingId.value = null
  }
}

function rowActions(row: APIKey): DropdownOption[] {
  // Revoked keys only keep cost view; config, optimize, import, and revoke drop out.
  // The optimization modal edits admin-only per-key overrides, so members
  // don't get that entry at all.
  const revoked = row.display_status === 'revoked'
  return [
    ...(revoked ? [] : [
      { label: t('apiKeys.editLimits'), key: 'edit' },
    ]),
    { label: t('costs.detail.viewCost'), key: 'look' },
    ...(revoked ? [] : [
      ...(authStore.isAdmin ? [{ label: t('costOptimization.title'), key: 'optimize' }] : []),
      { label: t('ccswitch.importAction'), key: 'importCCSImport' },
      { type: 'divider', key: 'd' },
      { label: t('apiKeys.revoke'), key: 'delete', props: { style: 'color: var(--color-danger)' } },
    ]),
  ]
}

const columns = computed<DataTableColumns<APIKey>>(() => [
  {
    title: columnTitle(t('apiKeys.keyPrefixColumn'), t('apiKeys.keyPrefixColumn_tip')),
    key: 'key_prefix',
    minWidth: 180,
    render: (row) =>
      h('div', { class: 'prefix-cell' }, [
        h('span', { class: 'mono-cell' }, `${row.key_prefix}…`),
        h(
          NTooltip,
          { trigger: 'hover' },
          {
            trigger: () =>
              h(
                NButton,
                {
                  size: 'tiny',
                  quaternary: true,
                  circle: true,
                  loading: revealingId.value === row.id,
                  onClick: () => copyPlaintext(row),
                },
                { icon: () => h(Copy, { size: 14 }) },
              ),
            default: () => t('apiKeys.copyFullKey'),
          },
        ),
      ]),
  },
  // The owning-account column only means something across accounts — a
  // member's list is always entirely their own.
  ...(authStore.isAdmin
    ? [{
        title: columnTitle(t('apiKeys.ownerUserColumn'), t('apiKeys.ownerUserColumn_tip')),
        key: 'owner_username',
        minWidth: 110,
        render: (row: APIKey) => row.owner_username || '—',
      }]
    : []),
  {
    title: columnTitle(t('apiKeys.remarkColumn'), t('apiKeys.remarkColumn_tip')),
    key: 'remark',
    minWidth: 160,
    render: (row) => row.remark || '—',
  },
  {
    title: columnTitle(t('apiKeys.statusColumn'), t('apiKeys.statusColumn_tip')),
    key: 'display_status',
    width: STATUS_COL_WIDTH,
    render: (row) =>
      h(NTag, { size: 'small', bordered: false, type: statusTagType(row.display_status) }, { default: () => statusLabel(row.display_status) }),
  },
  {
    title: columnTitle(t('apiKeys.budgetColumn'), t('apiKeys.budgetColumn_tip')),
    key: 'budget',
    width: 170,
    render: (row) => budgetCell(row),
  },
  {
    title: columnTitle(t('apiKeys.expiresColumn'), t('apiKeys.expiresColumn_tip')),
    key: 'expires_at',
    width: 200,
    render: (row) => expiresCell(row),
  },
  {
    title: t('common.actions'),
    key: 'actions',
    width: 60,
    align: 'center',
    render: (row) =>
      h(
        ResponsiveDropdown,
        {
          trigger: 'click',
          placement: 'bottom-end',
          triggerText: t('common.actions'),
          options: rowActions(row),
          onSelect: (key: string) => {
            if (key === 'edit') openEdit(row.id)
            else if (key === 'look') router.push(`/costs/keys/${row.id}`)
            else if (key === 'optimize') openOptimize(row)
            else if (key === 'delete') confirmRevoke(row)
            else if (key === 'importCCSImport') importKeyToCCS(row)
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
  }
])
</script>

<style scoped>
:deep(.prefix-cell) {
  display: flex;
  align-items: center;
  gap: 6px;
}
:deep(.mono-cell) {
  font-family: var(--font-mono, monospace);
  font-weight: 600;
  color: var(--color-text);
}
</style>
