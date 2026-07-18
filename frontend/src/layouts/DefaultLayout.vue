<!-- frontend/src/layouts/DefaultLayout.vue -->
<template>
  <div class="layout">
    <header>
      <span class="brand">
        <img :src="logo" alt="Yolorouter CE" class="brand-logo" />
        <span>Yolorouter CE</span>
      </span>
      <div class="header-right">
        <LocaleSwitcher />
        <n-dropdown :options="userMenuOptions" @select="onUserMenuSelect">
          <n-button quaternary>{{ authStore.username }}</n-button>
        </n-dropdown>
      </div>
    </header>
    <nav><!-- 导航项由各模块自己往里加 --></nav>
    <main><router-view /></main>

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
  </div>
</template>

<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useDialog, useMessage, type DropdownOption, type FormInst, type FormRules } from 'naive-ui'
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

// computed rather than a plain array so the labels stay in sync when the
// user switches locale without needing to re-open the dropdown.
const userMenuOptions = computed<DropdownOption[]>(() => [
  { label: t('auth.changePasswordTitle'), key: 'change-password' },
  { label: t('auth.logout'), key: 'logout' },
])

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
.brand {
  display: inline-flex;
  align-items: center;
  gap: 0.5rem;
}
.brand-logo {
  height: 1.5rem;
  width: auto;
}
.header-right {
  display: inline-flex;
  align-items: center;
  gap: 0.75rem;
}
</style>
