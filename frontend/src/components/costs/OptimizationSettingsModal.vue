<!-- frontend/src/components/costs/OptimizationSettingsModal.vue
     The two global cost-optimization switches (custom system prompt + input
     compression), behind one modal so the CostOptimizationPage can lead with
     savings data instead of form fields. Each switch keeps its own three-state
     load + version-CAS save; they are independent settings that fail, retry,
     and save independently. Emits "saved" after a successful PUT so the page
     can refresh its CTA banner without re-reading. -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="t('costOptimization.modalTitle')"
    max-width="640px"
    :mask-closable="false"
    :close-on-esc="false"
    :back-label="t('common.back')"
  >
    <!-- Custom system prompt -->
    <section class="setting-block">
      <div class="setting-block__head">
        <HelpLabel :tip="t('costOptimization.cspDesc')">{{ t('costOptimization.cspSubTitle') }}</HelpLabel>
      </div>
      <p class="setting-block__desc">{{ t('costOptimization.cspDesc') }}</p>
      <div v-if="cspLoad === 'loading'" class="setting-block__state">{{ t('common.loading') }}</div>
      <div v-else-if="cspLoad === 'error'" class="setting-block__state setting-block__state--err">
        <span>{{ t('costOptimization.loadFailed') }}</span>
        <NButton size="small" @click="loadCSP">{{ t('costOptimization.retry') }}</NButton>
      </div>
      <template v-else>
        <NForm label-placement="left" :show-require-mark="false">
          <NFormItem path="enabled">
            <template #label>
              <HelpLabel :tip="t('costOptimization.enabled_tip')">{{ t('costOptimization.enabled') }}</HelpLabel>
            </template>
            <NSwitch v-model:value="cspForm.enabled" :disabled="!cspSetting" class="block-switch" @change="saveCSP" />
          </NFormItem>

        </NForm>
      </template>
    </section>

    <!-- Input compression -->
    <section class="setting-block">
      <div class="setting-block__head">
        <HelpLabel :tip="t('costOptimization.inputCompression.titleTip')">
          {{ t('costOptimization.inputCompression.title') }}
        </HelpLabel>
      </div>
      <p class="setting-block__desc">{{ t('costOptimization.inputCompression.desc') }}</p>
      <div v-if="icLoad === 'loading'" class="setting-block__state">{{ t('common.loading') }}</div>
      <div v-else-if="icLoad === 'error'" class="setting-block__state setting-block__state--err">
        <span>{{ t('costOptimization.loadFailed') }}</span>
        <NButton size="small" @click="loadIC">{{ t('costOptimization.retry') }}</NButton>
      </div>
      <template v-else>
        <NForm label-placement="left" :show-require-mark="false">
          <NFormItem path="enabled">
            <template #label>
              <HelpLabel :tip="t('costOptimization.inputCompression.enabledTip')">{{ t('costOptimization.enabled') }}</HelpLabel>
            </template>
            <NSwitch v-model:value="icForm.enabled" class="block-switch" :disabled="!icSetting" @change="saveIC" />
          </NFormItem>
        </NForm>
      </template>
    </section>

    <!-- No footer: each switch auto-saves on toggle, so there's nothing to
         confirm. Dismissal is the desktop card's × / mobile drawer's back
         arrow. The empty slot overrides ModalDrawer's built-in footer. -->
    <template #footer><span /></template>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NForm, NFormItem, NSwitch, NButton, useMessage } from 'naive-ui'
import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import { APIError, displayMessage } from '../../api/client'
import {
  getCustomSystemPrompt,
  updateCustomSystemPrompt,
  getInputCompression,
  updateInputCompression,
  type CustomSystemPromptSetting,
  type InputCompressionSetting,
} from '../../api/systemSettings'
import { CUSTOM_SYSTEM_PROMPT_CONFLICT, INPUT_COMPRESSION_CONFLICT } from '../../api/errcodes'
import { defaultConcisePrompt } from '../../utils/concisePrompt'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{
  'update:show': [value: boolean]
  saved: []
}>()

// ModalDrawer owns a v-model:show; bridge it to this component's existing
// :show / @update:show contract so the parent doesn't have to change.
const showModel = computed({
  get: () => props.show,
  set: (v) => emit('update:show', v),
})

const { t, locale } = useI18n()
const message = useMessage()

// Mirrors internal/service/system_settings_service.go's MaxCustomSystemPromptLen
// so the client rejects oversized input before the round-trip. Same cap drives
// the per-key customSystemPromptRule and CustomPromptEditor's counter.
const MAX_CUSTOM_SYSTEM_PROMPT_LEN = 2000

// Iterate the string as Unicode code points without materializing an array
// (Array.from would allocate per check). The backend counts runes, so a
// surrogate-pair emoji still tallies as one character here — matching the
// per-key CSP rule and CustomPromptEditor's live counter.
function runeCount(s: string): number {
  let n = 0
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  for (const _ of s) n++
  return n
}

// CSP state — three-state load keeps the form hidden until the GET resolves,
// so a failed load can't expose editable defaults that would overwrite the
// real row on save.
const cspLoad = ref<'loading' | 'error' | 'loaded'>('loading')
const cspSetting = ref<CustomSystemPromptSetting | null>(null)
const cspForm = reactive({ enabled: false, text: '', version: 0 })
const cspSaving = ref(false)

async function loadCSP() {
  cspLoad.value = 'loading'
  try {
    const s = await getCustomSystemPrompt()
    cspSetting.value = s
    cspForm.enabled = s.enabled
    cspForm.text = defaultConcisePrompt(t, locale.value)
    cspForm.version = s.version
    cspLoad.value = 'loaded'
  } catch (err) {
    cspSetting.value = null
    cspLoad.value = 'error'
    if (!(err instanceof APIError)) message.error(displayMessage(err, t))
  }
}
async function saveCSP() {
  // Hard guard: never let a save fire before a successful GET — otherwise a
  // click during the error state could submit the empty defaults.
  if (!cspSetting.value || cspSaving.value) return
  // Client-side validation mirroring the backend (errcode 11010 too-long /
  // 11011 empty-when-enabled) so the user gets immediate feedback instead
  // of a round-trip rejection. Rune count matches the server's
  // MaxCustomSystemPromptLen cap and the per-key CSP rule.
  if (cspForm.enabled && !cspForm.text.trim()) {
    message.error(t('costOptimization.emptyTextError'))
    // The @change already flipped the switch optimistically; a rejected save
    // must not leave the toggle ON while the backend stays OFF, so re-sync it
    // to the last authoritative value.
    cspForm.enabled = cspSetting.value.enabled
    return
  }
  // Length only matters when the prompt is active — disabling must always be
  // allowed even if the stored text somehow exceeds the cap, otherwise the
  // revert below would snap the switch back on and trap the setting.
  if (cspForm.enabled && runeCount(cspForm.text) > MAX_CUSTOM_SYSTEM_PROMPT_LEN) {
    message.error(t('costOptimization.tooLongError'))
    cspForm.enabled = cspSetting.value.enabled
    return
  }
  cspSaving.value = true
  try {
    const updated = await updateCustomSystemPrompt({
      enabled: cspForm.enabled,
      text: cspForm.text,
      version: cspForm.version,
    })
    // Adopt the server's new version so a second save uses the right base.
    cspSetting.value = updated
    cspForm.version = updated.version
    cspForm.enabled = updated.enabled
    cspForm.text = updated.text
    message.success(t('costOptimization.saved'))
    emit('saved')
  } catch (err) {
    if (err instanceof APIError && err.code === CUSTOM_SYSTEM_PROMPT_CONFLICT) {
      // Concurrent edit — surface and reload authoritative state (which also
      // re-syncs the switch).
      message.error(t('costOptimization.conflict'))
      void loadCSP()
    } else {
      message.error(displayMessage(err, t))
      // A rejected save (network/500) must not leave the optimistically-toggled
      // switch ON while the backend stays OFF — re-sync it to the last
      // authoritative value, matching the client-side validation branches.
      if (cspSetting.value) cspForm.enabled = cspSetting.value.enabled
    }
  } finally {
    cspSaving.value = false
  }
}

// IC state — same three-state + version-CAS pattern, independent of CSP.
const icLoad = ref<'loading' | 'error' | 'loaded'>('loading')
const icSetting = ref<InputCompressionSetting | null>(null)
const icForm = reactive({ enabled: false, version: 0 })
const icSaving = ref(false)

async function loadIC() {
  icLoad.value = 'loading'
  try {
    const s = await getInputCompression()
    icSetting.value = s
    icForm.enabled = s.enabled
    icForm.version = s.version
    icLoad.value = 'loaded'
  } catch (err) {
    icSetting.value = null
    icLoad.value = 'error'
    if (!(err instanceof APIError)) message.error(displayMessage(err, t))
  }
}

async function saveIC() {
  if (!icSetting.value || icSaving.value) return
  icSaving.value = true
  try {
    const updated = await updateInputCompression({
      enabled: icForm.enabled,
      version: icForm.version,
    })
    icSetting.value = updated
    icForm.version = updated.version
    icForm.enabled = updated.enabled
    message.success(t('costOptimization.saved'))
    emit('saved')
  } catch (err) {
    if (err instanceof APIError && err.code === INPUT_COMPRESSION_CONFLICT) {
      message.error(t('costOptimization.inputCompression.conflict'))
      void loadIC()
    } else {
      message.error(displayMessage(err, t))
    }
  } finally {
    icSaving.value = false
  }
}

// Reload both whenever the modal opens — picks up external edits made since
// the last view and resets any stale error state.
watch(
  () => props.show,
  (v) => {
    if (v) {
      void loadCSP()
      void loadIC()
    }
  },
  { immediate: true },
)
</script>

<style scoped>
.setting-block + .setting-block {
  margin-top: var(--space-5);
}
.setting-block__head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-bottom: var(--space-2);
  font-weight: 700;
  color: var(--color-text);
}
.setting-block__desc {
  margin: 0 0 var(--space-3);
  color: var(--color-text-secondary);
  line-height: 1.6;
}
.setting-block__state {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-4) 0;
}
.setting-block__state--err {
  color: var(--color-text-secondary);
}
.setting-block__foot {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: var(--space-3);
  margin-top: var(--space-2);
}
/* Right-align the toggle within its form-item row without hard-coding a pixel
   offset against the card width. */
.block-switch {
  margin-left: auto;
}
</style>
