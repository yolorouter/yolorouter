<template>
  <AuthCard :title="t('auth.setupTitle')" :subtitle="t('auth.setupSubtitle')">
    <n-form ref="formRef" class="auth-form" :model="form" :rules="rules">
      <n-form-item path="username" :label="t('auth.username')">
        <n-input v-model:value="form.username" size="large" :disabled="submitting" @keyup.enter="onSubmit" />
      </n-form-item>
      <n-form-item path="password" :label="t('auth.password')">
        <n-input
          v-model:value="form.password"
          type="password"
          show-password-on="click"
          size="large"
          :disabled="submitting"
          @keyup.enter="onSubmit"
        />
      </n-form-item>
      <n-form-item path="confirmPassword" :label="t('auth.confirmPassword')">
        <n-input
          v-model:value="form.confirmPassword"
          type="password"
          show-password-on="click"
          size="large"
          :disabled="submitting"
          @keyup.enter="onSubmit"
        />
      </n-form-item>
      <n-button type="primary" block size="large" :loading="submitting" @click="onSubmit">
        {{ t('auth.createButton') }}
      </n-button>
    </n-form>
  </AuthCard>
</template>

<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useAuthStore } from '../../store/auth'
import { APIError, displayMessage } from '../../api/client'
import { ACCOUNT_SETUP_ALREADY_DONE } from '../../api/errcodes'
import { passwordStrengthRule, confirmPasswordRule, usernameFormatRule } from '../../utils/authValidators'
import AuthCard from '../../components/AuthCard.vue'

const { t } = useI18n()
const router = useRouter()
const authStore = useAuthStore()
const message = useMessage()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const form = reactive({ username: '', password: '', confirmPassword: '' })

// computed rather than a plain object so validation messages stay in the
// current locale if the user switches language while this page is open —
// AuthCard's language <select> renders on this very page.
const rules = computed<FormRules>(() => ({
  username: [usernameFormatRule(t)],
  password: [passwordStrengthRule(t)],
  confirmPassword: [confirmPasswordRule(t, () => form.password)],
}))

async function onSubmit() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  submitting.value = true
  try {
    await authStore.setup(form.username, form.password)
    router.push('/')
  } catch (err) {
    if (err instanceof APIError && err.code === ACCOUNT_SETUP_ALREADY_DONE) {
      // Someone else (a concurrent tab, or a retry after this exact
      // request actually succeeded server-side but the response was
      // lost) already completed setup. This response is itself
      // authoritative proof of that — set the flag directly rather than
      // round-tripping through checkState(), which could fail
      // independently (a second, unrelated network error) and leave the
      // user stuck on /setup with no way to reach /login.
      authStore.markInitialized()
      message.error(displayMessage(err, t))
      router.push('/login')
      return
    }
    message.error(displayMessage(err, t))
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.auth-form {
  margin-top: 24px;
}
</style>
