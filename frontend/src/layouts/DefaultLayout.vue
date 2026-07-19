<!-- frontend/src/layouts/DefaultLayout.vue -->
<template>
  <n-layout has-sider class="app-shell">
    <n-layout-sider
      :collapsed="collapsed"
      :collapsed-width="64"
      :width="220"
      collapse-mode="width"
      show-trigger="bar"
      bordered
      :native-scrollbar="false"
      class="layout-sider"
      @update:collapsed="(v: boolean) => (collapsed = v)"
    >
      <div class="sidebar-inner">
        <RouterLink to="/" class="sidebar-logo" :class="{ 'sidebar-logo--collapsed': collapsed }">
          <img :src="logo" alt="" width="26" />
          <span v-if="!collapsed" class="sidebar-logo__title">Yolorouter CE</span>
        </RouterLink>

        <div class="sidebar-nav-main">
          <SidebarNav :items="navItems" :collapsed="collapsed" />
        </div>

        <div style="flex: 1" />

        <div class="sidebar-bottom">
          <div v-if="!collapsed" class="sidebar-locale">
            <LocaleSwitcher />
          </div>
          <n-dropdown :options="userMenuOptions" placement="top-start" @select="onUserMenuSelect">
            <button class="sidebar-user" :class="{ 'sidebar-user--collapsed': collapsed }">
              <span class="sidebar-user__avatar">{{ userInitial }}</span>
              <span v-if="!collapsed" class="sidebar-user__name">{{ authStore.username }}</span>
            </button>
          </n-dropdown>
        </div>
      </div>
    </n-layout-sider>

    <n-layout class="layout-main">
      <n-layout-content>
        <div class="layout-content">
          <router-view />
        </div>
      </n-layout-content>
    </n-layout>

    <n-modal
      v-model:show="showChangePassword"
      preset="card"
      :title="t('auth.changePasswordTitle')"
      style="max-width: 400px"
      @after-leave="resetChangePasswordForm"
    >
      <n-form ref="changePasswordFormRef" :model="changePasswordForm" :rules="changePasswordRules">
        <n-form-item path="currentPassword" :label="t('auth.currentPassword')">
          <n-input v-model:value="changePasswordForm.currentPassword" type="password" show-password-on="click" />
        </n-form-item>
        <n-form-item path="newPassword" :label="t('auth.newPassword')">
          <n-input v-model:value="changePasswordForm.newPassword" type="password" show-password-on="click" />
        </n-form-item>
        <n-form-item path="confirmNewPassword" :label="t('auth.confirmNewPassword')">
          <n-input v-model:value="changePasswordForm.confirmNewPassword" type="password" show-password-on="click" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showChangePassword = false">{{ t('common.cancel') }}</n-button>
          <n-button type="primary" :loading="changingPassword" @click="onChangePasswordSubmit">
            {{ t('auth.changePasswordButton') }}
          </n-button>
        </n-space>
      </template>
    </n-modal>
  </n-layout>
</template>

<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useDialog, useMessage, type DropdownOption, type FormInst, type FormRules } from 'naive-ui'
import SidebarNav, { type NavItem } from '../components/SidebarNav.vue'
import { useAuthStore } from '../store/auth'
import { APIError, displayMessage } from '../api/client'
import { ACCOUNT_SESSION_INVALID } from '../api/errcodes'
import { passwordStrengthRule, confirmPasswordRule } from '../utils/authValidators'
import LocaleSwitcher from '../components/LocaleSwitcher.vue'
import logo from '../assets/logo.svg'

const { t } = useI18n()
const router = useRouter()
const authStore = useAuthStore()
const dialog = useDialog()
const message = useMessage()

const collapsed = ref(false)

// computed rather than a plain array so the labels stay in sync when the
// user switches locale without needing to re-open the dropdown.
const navItems = computed<NavItem[]>(() => [
  { key: 'dashboard', label: t('common.dashboard'), icon: 'LayoutGrid', to: '/' },
  { key: 'providers', label: t('providers.pageTitle'), icon: 'Server', to: '/providers' },
  { key: 'models', label: t('models.pageTitle'), icon: 'Box', to: '/models' },
])

// computed rather than a plain array so the labels stay in sync when the
// user switches locale without needing to re-open the dropdown.
const userMenuOptions = computed<DropdownOption[]>(() => [
  { label: t('auth.changePasswordTitle'), key: 'change-password' },
  { label: t('auth.logout'), key: 'logout' },
])

// A single-character avatar fallback (no avatar upload in this admin
// tool) — uppercased so a lowercase username still reads as a deliberate
// initial, not a truncation artifact.
const userInitial = computed(() => (authStore.username?.[0] ?? '?').toUpperCase())

function onUserMenuSelect(key: string) {
  if (key === 'change-password') {
    showChangePassword.value = true
  } else if (key === 'logout') {
    dialog.warning({
      title: t('auth.logoutConfirmTitle'),
      content: t('auth.logoutConfirmContent'),
      positiveText: t('auth.logout'),
      negativeText: t('common.cancel'),
      onPositiveClick: async () => {
        try {
          await authStore.logout()
        } catch (err) {
          if (err instanceof APIError && err.code === ACCOUNT_SESSION_INVALID) {
            // The session was already gone server-side — api/auth.ts's
            // session-invalid handling already cleared local auth state
            // and navigated to /login, so there's nothing left to do here.
            return
          }
          message.error(displayMessage(err, t))
          return
        }
        router.push('/login')
      },
    })
  }
}

const showChangePassword = ref(false)
const changingPassword = ref(false)
const changePasswordFormRef = ref<FormInst | null>(null)
const changePasswordForm = reactive({ currentPassword: '', newPassword: '', confirmNewPassword: '' })

// Cleared whenever the modal fully closes (@after-leave, below — fires for
// every close path: cancel, backdrop click, Esc, and the success path's
// own showChangePassword = false), not just on successful submit. Plain
// reactive state that outlives the modal being open would otherwise let
// anyone who regains access to an already-logged-in tab reopen the modal
// and read back a previously-typed password via "show password".
function resetChangePasswordForm() {
  changePasswordForm.currentPassword = ''
  changePasswordForm.newPassword = ''
  changePasswordForm.confirmNewPassword = ''
}

// computed rather than a plain object so the messages stay in the current
// locale if the user switches language while the modal happens to be open
// — matches userMenuOptions above.
const changePasswordRules = computed<FormRules>(() => ({
  currentPassword: [{ required: true, message: t('auth.fieldRequired'), trigger: ['blur', 'input'] }],
  newPassword: [passwordStrengthRule(t)],
  confirmNewPassword: [confirmPasswordRule(t, () => changePasswordForm.newPassword)],
}))

async function onChangePasswordSubmit() {
  try {
    await changePasswordFormRef.value?.validate()
  } catch {
    return
  }

  dialog.warning({
    title: t('auth.changePasswordConfirmTitle'),
    content: t('auth.changePasswordConfirmContent'),
    positiveText: t('auth.changePasswordButton'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      changingPassword.value = true
      try {
        await authStore.changePassword(changePasswordForm.currentPassword, changePasswordForm.newPassword)
        showChangePassword.value = false
        message.success(t('auth.changePasswordSuccess'))
        router.push('/login')
      } catch (err) {
        if (err instanceof APIError && err.code === ACCOUNT_SESSION_INVALID) {
          // api/auth.ts's session-invalid handling already cleared local
          // auth state and navigated to /login — just close this modal
          // and explain why, instead of leaving it open on top of the
          // login page.
          showChangePassword.value = false
          message.error(err.message)
        } else {
          message.error(displayMessage(err, t))
        }
      } finally {
        changingPassword.value = false
      }
    },
  })
}
</script>

<style scoped>
.layout-sider {
  background-color: var(--color-sidebar);
}

.sidebar-inner {
  display: flex;
  flex-direction: column;
  height: 100dvh;
  overflow: hidden;
}

.sidebar-logo {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  height: 64px;
  padding: 0 16px;
  color: var(--color-text);
  font-weight: 700;
}

.sidebar-logo--collapsed {
  justify-content: center;
  padding: 0;
}

.sidebar-nav-main {
  margin-top: var(--space-2);
}

.sidebar-bottom {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: 0 16px var(--space-4);
}

.sidebar-locale {
  display: flex;
}

.sidebar-user {
  display: flex;
  align-items: center;
  gap: 10px;
  height: 44px;
  padding: 0 8px;
  border: none;
  border-radius: var(--radius-md);
  background: transparent;
  color: var(--color-text);
  font: inherit;
  cursor: pointer;
  transition: background var(--duration-fast) var(--ease-out);
}

.sidebar-user:hover {
  background: var(--color-surface-hover);
}

.sidebar-user--collapsed {
  justify-content: center;
  padding: 0;
}

.sidebar-user__avatar {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  flex-shrink: 0;
  border-radius: var(--radius-full);
  background: var(--color-accent);
  color: #fff;
  font-size: var(--text-xs);
  font-weight: 700;
}

.sidebar-user__name {
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
  font-size: var(--text-sm);
  font-weight: 600;
}

.layout-main {
  background: transparent;
}

.layout-content {
  height: 100dvh;
  overflow: auto;
}

@media (max-width: 640px) {
  .sidebar-bottom {
    padding: 0 6px var(--space-4);
  }
}
</style>
