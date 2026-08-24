<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="t('costOptimization.title')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :confirm-text="t('apiKeys.save')"
    :cancel-text="t('apiKeys.cancel')"
    :loading="saving"
    :back-label="t('common.back')"
    @confirm="onSave"
  >
    <div v-if="loading" class="loading-row">{{ t('common.loading') }}</div>
    <n-form
      v-else
      ref="formRef"
      require-mark-placement="left"
      :model="form"
      :rules="rules"
      label-placement="top"
    >
      <n-form-item>
        <template #label>
          <HelpLabel :tip="t('apiKeys.cspOverride_tip')">{{ t('costOptimization.cspSubTitle') }}</HelpLabel>
        </template>
        <div>
          <n-radio-group :value="cspMode" @update:value="onCspModeChange">
            <n-radio value="inherit">{{ t('apiKeys.cspOverrideInherit') }}</n-radio>
            <n-radio value="on">{{ t('apiKeys.cspModeOn') }}</n-radio>
            <n-radio value="off">{{ t('apiKeys.cspModeOff') }}</n-radio>
          </n-radio-group>
          <p class="mode-hint">{{ cspHint }}</p>
        </div>
      </n-form-item>
      <n-form-item >
        <template #label>
          <HelpLabel :tip="t('apiKeys.compressOverride_tip')">{{ t('apiKeys.compressAction') }}</HelpLabel>
        </template>
        <div>        
          <n-radio-group :value="compressMode" @update:value="onCompressModeChange">
            <n-radio value="inherit">{{ t('apiKeys.compressOverrideInherit') }}</n-radio>
            <n-radio value="on">{{ t('apiKeys.compressModeOn') }}</n-radio>
            <n-radio value="off">{{ t('apiKeys.compressModeOff') }}</n-radio>
          </n-radio-group>
          <p class="mode-hint">{{ compressHint }}</p>
        </div>
      </n-form-item>

    </n-form>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NRadio, NRadioGroup, useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useApiKeysStore } from '../../store/apiKeys'
import { displayMessage, APIError } from '../../api/client'
import { getAPIKey, type APIKey } from '../../api/apiKeys'
import { API_KEY_CONFLICT } from '../../api/errcodes'
import { customSystemPromptRule } from '../../utils/apiKeyValidators'
import { defaultConcisePrompt } from '../../utils/concisePrompt'
import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'

// The parent passes only the key id and remounts via :key="apiKeyId" on each
// open, so onMounted fires once per open and performs the authoritative GET
// (the list row's CSP snapshot may already be stale by the time the modal is
// opened — another admin/tab could have edited the same key).
const props = defineProps<{
  show: boolean
  apiKeyId: number
}>()
const emit = defineEmits<{ (e: 'update:show', v: boolean): void; (e: 'saved'): void }>()

// ModalDrawer owns a v-model:show; bridge it to this component's existing
// :show / @update:show contract so the parent doesn't have to change.
const showModel = computed({
  get: () => props.show,
  set: (v) => emit('update:show', v),
})

const { t, locale } = useI18n()
const message = useMessage()
const store = useApiKeysStore()

const formRef = ref<FormInst | null>(null)
const loading = ref(true)
const saving = ref(false)
// CAS token captured from the authoritative GET — sent back as
// expected_updated_at on save. A 409 means another writer bumped it after our
// read; we re-GET and surface the conflict rather than overwriting.
const expectedUpdatedAt = ref<string | null>(null)

const form = reactive({
  custom_system_prompt_enabled_override: false,
  custom_system_prompt_enabled: false,
  custom_system_prompt: '',
  compress_enabled_override: false,
  compress_enabled: false,
})

const rules = computed<FormRules>(() => ({
  // CSP text is required only when override is active and enabled; the 2000
  // rune cap mirrors the service layer's MaxCustomSystemPromptLen.
  custom_system_prompt: customSystemPromptRule(
    t,
    form.custom_system_prompt_enabled_override,
    form.custom_system_prompt_enabled,
  ),
}))

// cspMode bridges the three-way radio (inherit / on / off) to the two
// underlying boolean fields the API expects (override + enabled).
type CspMode = 'inherit' | 'on' | 'off'
const cspMode = computed<CspMode>(() => {
  if (!form.custom_system_prompt_enabled_override) return 'inherit'
  return form.custom_system_prompt_enabled ? 'on' : 'off'
})
function onCspModeChange(mode: CspMode) {
  if (mode === 'inherit') {
    form.custom_system_prompt_enabled_override = false
  } else {
    form.custom_system_prompt_enabled_override = true
    form.custom_system_prompt_enabled = mode === 'on'
  }
}

// compressMode is the independent counterpart of cspMode: input compression
// and the custom system prompt are two separate per-key overrides, so this
// modal edits each with its own three-way radio rather than deriving one from
// the other.
type CompressMode = 'inherit' | 'on' | 'off'
const compressMode = computed<CompressMode>(() => {
  if (!form.compress_enabled_override) return 'inherit'
  return form.compress_enabled ? 'on' : 'off'
})
const compressHint = computed(() => {
  if (compressMode.value === 'inherit') return t('apiKeys.compressInheritHint')
  if (compressMode.value === 'on') return t('apiKeys.compressOnHint')
  return t('apiKeys.compressOffHint')
})
const cspHint = computed(() => {
  if (cspMode.value === 'inherit') return t('apiKeys.cspInheritHint')
  if (cspMode.value === 'on') return t('apiKeys.cspInheritHintOn')
  return t('apiKeys.cspOffHint')
})

function onCompressModeChange(mode: CompressMode) {
  if (mode === 'inherit') {
    form.compress_enabled_override = false
  } else {
    form.compress_enabled_override = true
    form.compress_enabled = mode === 'on'
  }
}

// fill adopts the authoritative GET response into the form and captures the
// CAS token. A failed GET keeps loading=true off and surfaces the error; the
// modal stays non-editable so a network blip can't trick the admin into
// saving empty defaults over the real row.
function fill(key: APIKey) {
  form.custom_system_prompt_enabled_override = key.custom_system_prompt_enabled_override
  form.custom_system_prompt_enabled = key.custom_system_prompt_enabled
  form.custom_system_prompt = defaultConcisePrompt(t, locale.value)
  form.compress_enabled_override = key.compress_enabled_override
  form.compress_enabled = key.compress_enabled
  expectedUpdatedAt.value = key.updated_at
}

async function load() {
  loading.value = true
  try {
    const key = await getAPIKey(props.apiKeyId)
    fill(key)
  } catch (err) {
    message.error(displayMessage(err, t))
    emit('update:show', false)
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load()
})

async function onSave() {
  if (saving.value || loading.value) return
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  saving.value = true
  try {

    await store.update(props.apiKeyId, {
      custom_system_prompt_enabled_override: form.custom_system_prompt_enabled_override,
      custom_system_prompt_enabled: form.custom_system_prompt_enabled,
      custom_system_prompt: form.custom_system_prompt,
      compress_enabled_override: form.compress_enabled_override,
      compress_enabled: form.compress_enabled,
      // expected_updated_at ties this save to the GET we opened with — a 409
      // means the row moved underneath us and we must re-read before saving.
      expected_updated_at: expectedUpdatedAt.value ?? undefined,
    })

    emit('saved')
  } catch (err) {
    if (err instanceof APIError && err.code === API_KEY_CONFLICT) {
      // Concurrent edit: another writer (or another tab) committed first.
      // Don't let the user fight theirs — surface the conflict and reload the
      // authoritative state so the next save uses a fresh CAS token.
      message.error(t('apiKeys.cspConflict'))
      await load()
    } else {
      message.error(displayMessage(err, t))
    }
  } finally {
    saving.value = false
  }
}
</script>

<style scoped>
.mode-hint {
  margin: 10px 0 0;
  padding: var(--space-2) var(--space-3);
  font-size: var(--text-sm);
  color: var(--color-text-muted);
  background: var(--color-bg-elevated, var(--color-bg));
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-md);
  width: 470px;
}

.loading-row {
  padding: var(--space-4) 0;
  color: var(--color-text-muted);
}
</style>
