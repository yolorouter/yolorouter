<!-- frontend/src/views/apikeys/ApiKeyListPage.vue -->
<template>
  <div class="apikeys-page">
    <PageHeader :eyebrow="t('apiKeys.eyebrow')" :title="t('apiKeys.pageTitle')" :description="t('apiKeys.pageDescription')">
      <template #actions>
        <n-button type="primary" @click="showCreate = true">
          <template #icon><Plus :size="16" /></template>
          {{ t('apiKeys.createButton') }}
        </n-button>
      </template>
    </PageHeader>

    <div class="apikeys-toolbar">
      <n-input
        :value="store.query"
        :placeholder="t('apiKeys.searchPlaceholder')"
        clearable
        class="apikeys-search"
        @update:value="onSearch"
      >
        <template #prefix><Search :size="14" /></template>
      </n-input>
    </div>

    <EmptyState v-if="!store.loading && store.list.length === 0" :title="t('apiKeys.listEmpty')">
      <template #action>
        <n-button type="primary" @click="showCreate = true">{{ t('apiKeys.createButton') }}</n-button>
      </template>
    </EmptyState>

    <div v-else class="data-table-wrapper">
      <n-data-table
        :columns="columns"
        :data="store.list"
        :loading="store.loading"
        :bordered="false"
        :single-line="false"
        :row-key="(row: APIKey) => row.id"
        :pagination="pagination"
        remote
      />
    </div>

    <CreateKeyModal v-model:show="showCreate" @created="onCreated" />
    <EditKeyDrawer v-if="editingId" :key="editingId" :show="showEdit" :api-key-id="editingId" @update:show="onEditShow" @saved="onSaved" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NSpace, NTag, useDialog, useMessage, type DataTableColumns, type PaginationProps } from 'naive-ui'
import { Plus, Search } from '@lucide/vue'
import { useApiKeysStore } from '../../store/apiKeys'
import { displayMessage } from '../../api/client'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { formatMicros } from '../../utils/money'
import type { APIKey } from '../../api/apiKeys'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import CreateKeyModal from '../../components/apikeys/CreateKeyModal.vue'
import EditKeyDrawer from '../../components/apikeys/EditKeyDrawer.vue'

const { t } = useI18n()
const dialog = useDialog()
const message = useMessage()
const store = useApiKeysStore()
const showCreate = ref(false)
const showEdit = ref(false)
const editingId = ref<number | null>(null)
let searchTimer: ReturnType<typeof setTimeout> | null = null

onMounted(() => {
  void store.fetchList().catch((err) => message.error(displayMessage(err, t)))
})

// Clear a pending debounced search if the user leaves the page inside the
// 300ms window — otherwise the timer fires after unmount, sending a request
// nobody sees and touching store/message state on a gone view.
onBeforeUnmount(() => {
  if (searchTimer) {
    clearTimeout(searchTimer)
    searchTimer = null
  }
})

async function reload() {
  try {
    await store.fetchList()
  } catch (err) {
    message.error(displayMessage(err, t))
  }
}

// Debounced search: a keystroke-level fetchList() would fire one request per
// character and race the shared store's lastFetchId guard needlessly.
function onSearch(v: string | null) {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    store.setQuery(v ?? '')
    void reload()
  }, 300)
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

// Cancel/X close the drawer via update:show=false — clear editingId too so
// v-if="editingId" flips off and the next openEdit (same row or another)
// remounts the drawer and re-runs onMounted/fill. Without this, reopening the
// same row would reuse the stale form from the previous open.
function onEditShow(v: boolean) {
  showEdit.value = v
  if (!v) editingId.value = null
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

function onCreated() {
  message.success(t('apiKeys.createSuccess'))
  void reload()
}

function onSaved() {
  showEdit.value = false
  // Reset editingId so the drawer unmounts (v-if="editingId"); reopening it
  // remounts and re-runs onMounted/fill instead of showing stale form state.
  editingId.value = null
  message.success(t('apiKeys.saveSuccess'))
  void reload()
}

const columns = computed<DataTableColumns<APIKey>>(() => [
  {
    title: columnTitle(t('apiKeys.keyPrefixColumn'), t('apiKeys.keyPrefixColumn_tip')),
    key: 'key_prefix',
    minWidth: 180,
    render: (row) => h('span', { class: 'mono-cell' }, `${row.key_prefix}…`),
  },
  {
    title: columnTitle(t('apiKeys.ownerColumn'), t('apiKeys.ownerColumn_tip')),
    key: 'owner_label',
    minWidth: 120,
    render: (row) => row.owner_label || '—',
  },
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
    // Actions column — no tooltip.
    title: t('common.actions'),
    key: 'actions',
    width: 150,
    render: (row) => {
      const revoked = row.display_status === 'revoked'
      return h(
        NSpace,
        { size: 'small' },
        {
          default: () => [
            h(NButton, { size: 'small', text: true, type: 'primary', onClick: () => openEdit(row.id) }, { default: () => t('apiKeys.editLimits') }),
            revoked
              ? null
              : h(NButton, { size: 'small', text: true, type: 'error', onClick: () => confirmRevoke(row) }, { default: () => t('apiKeys.revoke') }),
          ],
        },
      )
    },
  },
])
</script>

<style scoped>
.apikeys-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

.apikeys-toolbar {
  display: flex;
}

.apikeys-search {
  max-width: 360px;
}

:deep(.mono-cell) {
  font-family: var(--font-mono, monospace);
  font-weight: 600;
  color: var(--color-text);
}
</style>
