<!-- frontend/src/components/system/KeyAutoRecoveryModal.vue
     Global key-auto-recovery settings, opened from the admin sidebar's System
     group: the background loop's on/off switch and its probe interval in
     whole minutes. Load + version-CAS save mirrors the other system-settings
     modals (OptimizationSettingsModal / VisionFallbackModal): the GET is
     authoritative, the PUT carries its version, and a 409 means another admin
     committed first — surface it, reload the committed row, let the user
     review and save again. A successful save also re-reads the GET so the
     form reflects the committed row, not just what we asked the server to
     write. -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="t('keyAutoRecovery.title')"
    max-width="480px"
    :confirm-text="t('common.save')"
    :cancel-text="t('common.cancel')"
    :loading="saving"
    :confirm-disabled="load !== 'ready'"
    :back-label="t('common.back')"
    @confirm="onSave"
  >
    <p class="kar-desc">{{ t('keyAutoRecovery.desc') }}</p>
    <div v-if="load === 'loading'" class="kar-state">{{ t('common.loading') }}</div>
    <div v-else-if="load === 'error'" class="kar-state kar-state--err">
      <span>{{ t('keyAutoRecovery.loadFailed') }}</span>
      <NButton size="small" @click="loadSetting">{{ t('keyAutoRecovery.retry') }}</NButton>
    </div>
    <!-- Validation runs through NForm rules (blur trigger) so the red
         feedback appears inline on leaving the field; onSave re-validates
         via formRef.validate() before anything is sent. -->
    <!-- require-mark-placement="left": with the #label slot pattern the
         asterisk is a sibling after the slot content, so the default 'right'
         would render it after the ? icon ("Interval ? *"). 'left' moves it
         before the label ("* Interval ?"). -->
    <NForm v-else ref="formRef" :model="form" :rules="rules" require-mark-placement="left">
      <NFormItem path="enabled">
        <template #label>
          <HelpLabel :tip="t('keyAutoRecovery.enabledTip')">{{ t('keyAutoRecovery.enabled') }}</HelpLabel>
        </template>
        <NSwitch v-model:value="form.enabled" class="kar-switch" />
      </NFormItem>
      <NFormItem path="interval">
        <template #label>
          <HelpLabel :tip="t('keyAutoRecovery.intervalTip')">{{ t('keyAutoRecovery.interval') }}</HelpLabel>
        </template>
        <!-- No :min/:max/:precision on the input itself on purpose:
             NInputNumber clamps out-of-range values silently on blur and
             would round a decimal the same way, while this form must reject
             both with an explicit message (the rule below) so the admin
             knows what was wrong instead of discovering a changed value. -->
        <NInputNumber
          v-model:value="form.interval"
          class="kar-interval"
        />
      </NFormItem>
    </NForm>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  NButton,
  NForm,
  NFormItem,
  NInputNumber,
  NSwitch,
  useMessage,
  type FormInst,
  type FormRules,
} from 'naive-ui'
import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import { APIError, displayMessage } from '../../api/client'
import { getKeyAutoRecovery, updateKeyAutoRecovery } from '../../api/systemSettings'
import { KEY_AUTO_RECOVERY_CONFLICT } from '../../api/errcodes'
import {
  checkIntervalMinutes,
  intervalIssueMessageKey,
  keyRecoveryFormFromSetting,
  keyRecoverySavePlan,
  type KeyRecoveryForm,
} from '../../utils/keyAutoRecovery'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ 'update:show': [boolean] }>()

const showModel = computed({
  get: () => props.show,
  set: (v) => emit('update:show', v),
})

const { t } = useI18n()
const message = useMessage()

// Three-state load, same as the sibling settings modals: the form stays
// hidden until the GET resolves so a failed load can't expose editable
// defaults that would overwrite the real row on save.
const load = ref<'loading' | 'error' | 'ready'>('loading')
const saving = ref(false)
const version = ref(0)
const formRef = ref<FormInst | null>(null)
const form = reactive<KeyRecoveryForm>({ enabled: false, interval: null })

// The rule's message is picked by the pure validator's issue, so the bounds
// live in exactly one place (utils/keyAutoRecovery). The computed reads
// form.interval through intervalValidationMessage(), so every keystroke
// rebuilds the rule with the message for the value now in the box (empty for
// a valid one) — a message captured once would go stale after the first
// edit, the same trap confirmPasswordRule avoids by re-reading its getter at
// validation time.
const rules = computed<FormRules>(() => ({
  interval: [
    {
      required: true,
      validator: (_rule, value: number | null) => checkIntervalMinutes(value).ok,
      message: intervalValidationMessage(),
      trigger: ['blur'],
    },
  ],
}))

function intervalValidationMessage(): string {
  const check = checkIntervalMinutes(form.interval)
  return check.ok ? '' : t(intervalIssueMessageKey(check.issue))
}

async function loadSetting() {
  load.value = 'loading'
  try {
    const s = await getKeyAutoRecovery()
    const next = keyRecoveryFormFromSetting(s)
    form.enabled = next.enabled
    form.interval = next.interval
    version.value = s.version
    formRef.value?.restoreValidation()
    load.value = 'ready'
  } catch (err) {
    load.value = 'error'
    if (!(err instanceof APIError)) message.error(displayMessage(err, t))
  }
}

// Reload whenever the modal opens — picks up external edits made since the
// last view and resets any stale error state.
watch(
  () => props.show,
  (visible) => {
    if (visible) void loadSetting()
  },
  { immediate: true },
)

async function onSave() {
  // Nothing loaded means nothing valid to save: a PUT here would carry
  // version 0 and bounce off the backend's version check anyway.
  if (load.value !== 'ready' || saving.value) return
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  const plan = keyRecoverySavePlan({ enabled: form.enabled, interval: form.interval }, version.value)
  if (!plan.ok) {
    // Defensive double lock: validate() already gates on the same rule, so
    // this only fires if the form ref was somehow absent above.
    message.error(t(intervalIssueMessageKey(plan.issue)))
    return
  }
  saving.value = true
  try {
    await updateKeyAutoRecovery(plan.payload)
    message.success(t('keyAutoRecovery.saved'))
    // Re-read the committed row (not just the PUT echo) so the form shows
    // the authoritative state — the same authority contract as the GET on
    // open, and it refreshes the version for any subsequent save.
    await loadSetting()
  } catch (err) {
    if (err instanceof APIError && err.code === KEY_AUTO_RECOVERY_CONFLICT) {
      // Concurrent edit — surface it and reload the committed row (which
      // also re-syncs the form and its version).
      message.error(t('keyAutoRecovery.conflict'))
      void loadSetting()
    } else {
      message.error(displayMessage(err, t))
    }
  } finally {
    saving.value = false
  }
}
</script>

<style scoped lang="less">
.kar-desc {
  margin: 0 0 12px;
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
  line-height: 1.6;
}
.kar-state {
  display: flex;
  align-items: center;
  gap: 12px;
  color: var(--color-text-muted);
  font-size: var(--text-sm);
}
.kar-state--err {
  color: var(--color-text-secondary);
}
/* Right-align the toggle within its form-item row, matching
   OptimizationSettingsModal's block-switch treatment. */
.kar-switch {
  margin-left: auto;
}
.kar-interval {
  width: 100%;
}
</style>
