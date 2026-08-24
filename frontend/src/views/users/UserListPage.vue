<!-- frontend/src/views/users/UserListPage.vue
     Admin user directory. One flat list (company-internal deployments are
     small — no pagination, filters run client-side), showing where each
     account came from (local password vs. login providers), its role and
     status, and the usage aggregates (key count, lifetime spend). Actions:
     provision a local password member, enable/disable and promote/demote.
     There is deliberately no delete — accounts anchor keys and request
     history, so removal would orphan or falsify both; disabling is the
     terminal state. -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('users.eyebrow')" :title="t('users.pageTitle')" :description="t('users.pageDescription')">
      <template #actions>
        <n-button type="primary" @click="showCreate = true">
          <template #icon><Plus :size="16" /></template>
          {{ t('users.createButton') }}
        </n-button>
      </template>
    </PageHeader>

    <div class="filter-panel">
      <div class="filter-grid">
        <div class="filter-item filter-item--search">
          <n-input
            v-model:value="filter.query"
            :placeholder="t('users.searchPlaceholder')"
            clearable
            size="small"
          >
            <template #prefix><Search :size="14" /></template>
          </n-input>
        </div>
        <FilterSelectField
          v-model:value="filter.role"
          :label="t('users.roleColumn')"
          :options="roleOptions"
          :placeholder="t('users.filterRole')"
          size="small"
          width="100%"
        />
        <FilterSelectField
          v-model:value="filter.status"
          :label="t('users.statusColumn')"
          :options="statusOptions"
          :placeholder="t('users.filterStatus')"
          size="small"
          width="100%"
        />
      </div>
    </div>

    <div class="data-table-wrapper">
      <ResponsiveDataTable
        :columns="columns"
        :data="filteredUsers"
        :loading="loading"
        :scroll-x="1080"
        :row-key="(row: UserSummary) => row.id"
      >
        <template #empty>
          <EmptyState :icon="UsersRound" :title="t('users.listEmpty')">
            <template #action>
              <n-button type="primary" @click="showCreate = true">{{ t('users.createButton') }}</n-button>
            </template>
          </EmptyState>
        </template>
      </ResponsiveDataTable>
    </div>

    <CreateUserModal v-model:show="showCreate" @created="reload" />

    <ResetPasswordModal
      v-model:show="showReset"
      :user-id="resetTarget?.id ?? 0"
      :username="resetTarget?.username ?? ''"
      @reset="reload"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NTag, useDialog, useMessage, type DataTableColumns, type DropdownOption, type SelectOption } from 'naive-ui'
import { Search, MoreHorizontal, Plus, UsersRound } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import CreateUserModal from '../../components/users/CreateUserModal.vue'
import ResetPasswordModal from '../../components/users/ResetPasswordModal.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import ResponsiveDropdown from '../../components/common/ResponsiveDropdown.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { formatMicros } from '../../utils/money'
import { displayMessage } from '../../api/client'
import { listUsers, updateUserRole, updateUserStatus, USER_STATUS_ENABLED, type UserSummary } from '../../api/users'
import { useAuthStore } from '../../store/auth'

const { t } = useI18n()
const message = useMessage()
const dialog = useDialog()
const authStore = useAuthStore()

const users = ref<UserSummary[]>([])
const loading = ref(false)

const filter = reactive({
  query: '',
  role: null as string | null,
  status: null as string | null,
})

const roleOptions = computed<SelectOption[]>(() => [
  { label: t('users.roleAdmin'), value: 'admin' },
  { label: t('users.roleMember'), value: 'member' },
])
const statusOptions = computed<SelectOption[]>(() => [
  { label: t('users.statusEnabled'), value: 'enabled' },
  { label: t('users.statusDisabled'), value: 'disabled' },
])

const filteredUsers = computed(() => {
  const q = filter.query.trim().toLowerCase()
  return users.value.filter((u) => {
    if (q && !u.username.toLowerCase().includes(q) && !u.email.toLowerCase().includes(q) && !u.display_name.toLowerCase().includes(q)) {
      return false
    }
    if (filter.role && u.role !== filter.role) return false
    if (filter.status === 'enabled' && u.status !== USER_STATUS_ENABLED) return false
    if (filter.status === 'disabled' && u.status === USER_STATUS_ENABLED) return false
    return true
  })
})

async function reload() {
  loading.value = true
  try {
    const page = await listUsers()
    users.value = page.users
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void reload()
})

const showCreate = ref(false)

// Rows without actions: the acting admin's own account (the backend
// refuses self-operations) and the bootstrap account (the first-run setup
// anchor — it can never be disabled or demoted, and it is already an
// admin, so no action remains). Offering the entries would only produce a
// guaranteed error toast. Console-created local members are NOT covered:
// they are ordinary accounts with the full action set.
function hasNoActions(row: UserSummary): boolean {
  return row.username === authStore.username || row.is_bootstrap
}

function rowActions(row: UserSummary): DropdownOption[] {
  const enabled = row.status === USER_STATUS_ENABLED
  const items: DropdownOption[] = []
  // Password resets are the bootstrap administrator's alone (the backend
  // refuses everyone else), and only local accounts have a password.
  if (row.is_local && authStore.isBootstrap) {
    items.push({ label: t('users.resetPassword'), key: 'reset' })
    items.push({ type: 'divider', key: 'd0' })
  }
  items.push(
    row.role === 'admin'
      ? { label: t('users.demote'), key: 'demote' }
      : { label: t('users.promote'), key: 'promote' },
    { type: 'divider', key: 'd' },
    enabled
      ? { label: t('users.disable'), key: 'disable', props: { style: 'color: var(--color-danger)' } }
      : { label: t('users.enable'), key: 'enable' },
  )
  return items
}

// The reset target for the reset-password modal; null keeps it closed.
const resetTarget = ref<{ id: number; username: string } | null>(null)
const showReset = computed({
  get: () => resetTarget.value !== null,
  set: (v) => {
    if (!v) resetTarget.value = null
  },
})

function onAction(row: UserSummary, key: string) {
  if (key === 'reset') {
    resetTarget.value = { id: row.id, username: row.username }
    return
  }
  const confirmMap: Record<string, { title: string; content: string; run: () => Promise<null> }> = {
    disable: {
      title: t('users.disableConfirmTitle'),
      content: t('users.disableConfirmContent', { name: row.username }),
      run: () => updateUserStatus(row.id, 'disabled'),
    },
    enable: {
      title: t('users.enableConfirmTitle'),
      content: t('users.enableConfirmContent', { name: row.username }),
      run: () => updateUserStatus(row.id, 'enabled'),
    },
    promote: {
      title: t('users.promoteConfirmTitle'),
      content: t('users.promoteConfirmContent', { name: row.username }),
      run: () => updateUserRole(row.id, 'admin'),
    },
    demote: {
      title: t('users.demoteConfirmTitle'),
      content: t('users.demoteConfirmContent', { name: row.username }),
      run: () => updateUserRole(row.id, 'member'),
    },
  }
  const action = confirmMap[key]
  if (!action) return
  dialog.warning({
    title: action.title,
    content: action.content,
    positiveText: t('common.confirm'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await action.run()
        message.success(t('users.actionSuccess'))
      } catch (err) {
        message.error(displayMessage(err, t))
      }
      void reload()
    },
  })
}

function sourceCell(row: UserSummary) {
  if (row.is_local) {
    const tags = [h(NTag, { size: 'small', bordered: false }, { default: () => t('users.sourceLocal') })]
    if (row.is_bootstrap) {
      tags.push(h(NTag, { size: 'small', bordered: false, type: 'warning' },
        { default: () => t('users.sourceBootstrap') }))
    }
    return h('div', { style: 'display:flex; gap:4px; flex-wrap:wrap' }, tags)
  }
  if (!row.providers.length) {
    return '—'
  }
  return h('div', { style: 'display:flex; gap:4px; flex-wrap:wrap' },
    row.providers.map((name) =>
      h(NTag, { size: 'small', bordered: false, type: 'info' }, { default: () => name })))
}

const columns = computed<DataTableColumns<UserSummary>>(() => [
  {
    title: columnTitle(t('users.usernameColumn'), t('users.usernameColumn_tip')),
    key: 'username',
    minWidth: 150,
    render: (row) =>
      h('div', {}, [
        h('div', { style: 'font-weight: 600' }, row.username),
        row.display_name && row.display_name !== row.username
          ? h('div', { style: 'font-size: 12px; color: var(--color-text-tertiary)' }, row.display_name)
          : null,
      ]),
  },
  {
    title: columnTitle(t('users.emailColumn'), t('users.emailColumn_tip')),
    key: 'email',
    minWidth: 170,
    render: (row) => row.email || '—',
  },
  {
    title: columnTitle(t('users.sourceColumn'), t('users.sourceColumn_tip')),
    key: 'source',
    minWidth: 120,
    render: sourceCell,
  },
  {
    title: columnTitle(t('users.roleColumn'), t('users.roleColumn_tip')),
    key: 'role',
    width: 100,
    render: (row) =>
      h(NTag, { size: 'small', bordered: false, type: row.role === 'admin' ? 'warning' : 'default' },
        { default: () => (row.role === 'admin' ? t('users.roleAdmin') : t('users.roleMember')) }),
  },
  {
    title: columnTitle(t('users.statusColumn'), t('users.statusColumn_tip')),
    key: 'status',
    width: STATUS_COL_WIDTH,
    render: (row) =>
      h(NTag, { size: 'small', bordered: false, type: row.status === USER_STATUS_ENABLED ? 'success' : 'error' },
        { default: () => (row.status === USER_STATUS_ENABLED ? t('users.statusEnabled') : t('users.statusDisabled')) }),
  },
  {
    title: columnTitle(t('users.keyCountColumn'), t('users.keyCountColumn_tip')),
    key: 'key_count',
    width: 90,
    render: (row) => String(row.key_count),
  },
  {
    title: columnTitle(t('users.spendColumn'), t('users.spendColumn_tip')),
    key: 'spend_micros',
    width: 120,
    render: (row) => `¥${formatMicros(row.spend_micros, 2)}`,
  },
  {
    title: columnTitle(t('users.lastLoginColumn'), t('users.lastLoginColumn_tip')),
    key: 'last_login_at',
    width: 170,
    render: (row) => (row.last_login_at ? new Date(row.last_login_at).toLocaleString() : '—'),
  },
  {
    title: t('common.actions'),
    key: 'actions',
    width: 60,
    align: 'center',
    render: (row) =>
      hasNoActions(row)
        ? '—'
        : h(
            ResponsiveDropdown,
            {
              trigger: 'click',
              placement: 'bottom-end',
              triggerText: t('common.actions'),
              options: rowActions(row),
              onSelect: (key: string) => onAction(row, key),
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
  },
])
</script>
