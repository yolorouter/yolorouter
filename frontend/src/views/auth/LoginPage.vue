<template>
  <AuthCard :title="t('auth.loginTitle')" :subtitle="t('auth.loginSubtitle')">
    <n-form require-mark-placement="left" ref="formRef" class="auth-form" :model="form" :rules="rules">
      <n-form-item path="username">
        <template #label>
          <HelpLabel :tip="t('auth.username_tip')">{{ t('auth.username') }}</HelpLabel>
        </template>
        <n-input
          v-model:value="form.username"
          size="large"
          :disabled="submitting || lockedSecondsLeft > 0"
          @keyup.enter="onSubmit"
        />
      </n-form-item>
      <n-form-item path="password">
        <template #label>
          <HelpLabel :tip="t('auth.password_tip')">{{ t('auth.password') }}</HelpLabel>
        </template>
        <n-input
          v-model:value="form.password"
          type="password"
          show-password-on="click"
          size="large"
          :disabled="submitting || lockedSecondsLeft > 0"
          @keyup.enter="onSubmit"
        />
      </n-form-item>
      <n-alert v-if="displayedMessage" type="error" class="auth-error">{{ displayedMessage }}</n-alert>
      <n-button
        type="primary"
        block
        size="large"
        :loading="submitting"
        :disabled="lockedSecondsLeft > 0"
        @click="onSubmit"
      >
        {{ lockedSecondsLeft > 0 ? t('auth.lockedCountdown', { seconds: lockedSecondsLeft }) : t('auth.loginButton') }}
      </n-button>
    </n-form>
  </AuthCard>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import type { FormInst, FormRules } from 'naive-ui'
import { useAuthStore } from '../../store/auth'
import { APIError, displayMessage } from '../../api/client'
import { ACCOUNT_LOGIN_LOCKED, ACCOUNT_SESSION_INVALID } from '../../api/errcodes'
import { errcodeMessage } from '../../i18n'
import type { LoginLockedData } from '../../api/auth'
import AuthCard from '../../components/AuthCard.vue'
import HelpLabel from '../../components/HelpLabel.vue'

const { t } = useI18n()
const router = useRouter()
const authStore = useAuthStore()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const errorMessage = ref('')
// PRD §6.1.5: landing here because the session expired (the 24h TTL, or
// any mid-use AccountSessionInvalid) must show the ACCOUNT_SESSION_INVALID
// message — not a blank form with no explanation. consumeSessionExpiredNotice reads and
// clears the store flag in one step, so a later, unrelated login attempt
// on this same page instance doesn't keep re-showing a stale notice. Kept
// as a separate reactive flag (rather than writing the resolved string
// directly into errorMessage once) so the text stays in the current
// locale if the user switches language before it's dismissed.
const sessionExpiredNoticeShown = ref(authStore.consumeSessionExpiredNotice())
const displayedMessage = computed(() =>
  sessionExpiredNoticeShown.value ? errcodeMessage(ACCOUNT_SESSION_INVALID) : errorMessage.value,
)
const form = reactive({ username: '', password: '' })
const lockedSecondsLeft = ref(0)
let countdownTimer: ReturnType<typeof setInterval> | undefined

// computed rather than a plain object so validation messages stay in the
// current locale if the user switches language while this page is open —
// AuthCard's language <select> renders on this very page.
const rules = computed<FormRules>(() => ({
  username: [{ required: true, message: t('auth.fieldRequired'), trigger: ['blur', 'input'] }],
  password: [{ required: true, message: t('auth.fieldRequired'), trigger: ['blur', 'input'] }],
}))

// startCountdown takes a duration already computed from two server-clock
// values (AccountLoginLocked's locked_until minus the envelope's own
// timestamp — see the catch block below), never the browser's own
// Date.now(), for the STARTING point: comparing a server timestamp
// directly against the client's clock would be wrong by however far the
// two disagree. From there, `deadline` is purely a client-clock value
// (Date.now() plus that server-relative duration), so measuring elapsed
// time against it on each tick only ever compares the client's clock to
// itself.
//
// This recomputes secondsLeft from actual elapsed time on every tick
// rather than decrementing a counter once per setInterval callback:
// browsers throttle background-tab timers (Chrome caps a backgrounded
// tab's setInterval to ~1/min after a few minutes; OS sleep/wake pauses
// them entirely), so a tick-counting countdown drifts arbitrarily far
// from the real unlock time whenever the tab isn't in the foreground —
// recomputing from `deadline` on whatever tick does fire always reflects
// the true remaining time, including snapping straight to 0 if the
// deadline already passed while backgrounded.
function startCountdown(remainingSeconds: number) {
  clearCountdown()
  const deadline = Date.now() + Math.max(0, remainingSeconds) * 1000
  const tick = () => {
    lockedSecondsLeft.value = Math.max(0, Math.ceil((deadline - Date.now()) / 1000))
    if (lockedSecondsLeft.value === 0) {
      clearCountdown()
      // The lockout has now genuinely expired — the stale "account
      // locked" banner must go away on its own instead of lingering
      // above a button that looks and behaves like a normal login
      // button until the user submits again.
      errorMessage.value = ''
    }
  }
  tick()
  if (lockedSecondsLeft.value === 0) return
  countdownTimer = setInterval(tick, 1000)
}

function clearCountdown() {
  if (countdownTimer !== undefined) {
    clearInterval(countdownTimer)
    countdownTimer = undefined
  }
}

onBeforeUnmount(clearCountdown)

async function onSubmit() {
  errorMessage.value = ''
  sessionExpiredNoticeShown.value = false
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  submitting.value = true
  try {
    await authStore.login(form.username, form.password)
    router.push('/')
  } catch (err) {
    form.password = ''
    if (err instanceof APIError && err.code === ACCOUNT_LOGIN_LOCKED) {
      const data = err.data as LoginLockedData | undefined
      if (data?.locked_until) startCountdown(data.locked_until - err.timestamp)
    }
    errorMessage.value = displayMessage(err, t)
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.auth-form {
  margin-top: 24px;
}

.auth-error {
  margin-bottom: 12px;
}
</style>
