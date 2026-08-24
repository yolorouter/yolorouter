<!-- frontend/src/components/apikeys/CreateKeyModal.vue
     Two-step modal: form -> one-time plaintext reveal. The plaintext is the
     only chance to see the full key; closing without
     custom-system-prompt and compression are configured post-creation via the
     optimization modal. -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="step === 'form' ? t('apiKeys.createTitle') : t('apiKeys.plaintextTitle')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :back-label="t('common.back')"
    @after-leave="reset"
  >
    <n-form
      v-if="step === 'form'"
      ref="formRef"
      require-mark-placement="left"
      :model="form"
      :rules="rules"
      label-placement="top"
    >
      <n-form-item path="remark">
        <template #label>
          <HelpLabel :tip="t('apiKeys.remark_tip')">{{ t('apiKeys.remark') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.remark" type="textarea" :autosize="{ minRows: 2 }" :maxlength="200" />
      </n-form-item>
      <n-form-item v-if="authStore.isAdmin">
        <template #label>
          <HelpLabel :tip="t('apiKeys.modelScope_tip')">{{ t('apiKeys.modelScope') }}</HelpLabel>
        </template>
        <n-radio-group v-model:value="form.allow_all_models">
          <n-radio :value="true">{{ t('apiKeys.modelScopeAll') }}</n-radio>
          <n-radio :value="false">{{ t('apiKeys.modelScopeCustom') }}</n-radio>
        </n-radio-group>
      </n-form-item>
      <n-form-item v-if="authStore.isAdmin && !form.allow_all_models" path="model_ids">
        <template #label>
          <HelpLabel :tip="t('apiKeys.modelAllowlist_tip')">{{ t('apiKeys.modelAllowlist') }}</HelpLabel>
        </template>
        <FilterSelectField
          :value="form.model_ids"
          :label="t('apiKeys.modelAllowlist')"
          multiple
          filterable
          :clearable="false"
          :options="modelOptions"
          size="medium"
          :placeholder="t('apiKeys.modelAllowlist')"
          width="100%"
          class="w-full"
          @update:value="(v: number | number[] | null) => (form.model_ids = (v as number[] | null) ?? [])"
        />
      </n-form-item>
      <div :style="isMobile ? 'position: absolute; top: 12px; right: 10px;' : 'position: absolute; top: 17px; right: 60px;'">
        <NDatePicker v-model:value="form.expires_at" type="datetime" clearable class="full-width" :placeholder="t('apiKeys.selectExpiresAt')" />
      </div>

      <div v-if="authStore.isAdmin" class="limit-section">
        <div class="limit-section__label">{{ t('apiKeys.limitsSection') }}</div>
        <div class="limit-grid">
          <n-form-item>
            <template #label>
              <HelpLabel :tip="t('apiKeys.rpmLimit_tip')">{{ t('apiKeys.rpmLimit') }}</HelpLabel>
            </template>
            <n-input-number v-model:value="form.rpm_limit" :min="0" :placeholder="t('apiKeys.limitHint')" class="full-width" />
          </n-form-item>
          <n-form-item>
            <template #label>
              <HelpLabel :tip="t('apiKeys.tpmLimit_tip')">{{ t('apiKeys.tpmLimit') }}</HelpLabel>
            </template>
            <n-input-number v-model:value="form.tpm_limit" :min="0" :placeholder="t('apiKeys.limitHint')" class="full-width" />
          </n-form-item>
          <n-form-item>
            <template #label>
              <HelpLabel :tip="t('apiKeys.concurrencyLimit_tip')">{{ t('apiKeys.concurrencyLimit') }}</HelpLabel>
            </template>
            <n-input-number v-model:value="form.concurrency_limit" :min="0" :placeholder="t('apiKeys.limitHint')" class="full-width" />
          </n-form-item>
          <n-form-item>
            <template #label>
              <HelpLabel :tip="t('apiKeys.budgetLimit_tip')">{{ t('apiKeys.budgetLimit') }}</HelpLabel>
            </template>
            <n-input-number v-model:value="form.budget_amount" :min="0" :step="0.01" :placeholder="t('apiKeys.limitHint')" class="full-width" />
          </n-form-item>
        </div>
      </div>
    </n-form>

    <div v-else class="plaintext-step">
      <n-alert type="warning" :show-icon="true" class="plaintext-warning">
        {{ t('apiKeys.plaintextWarning') }}
      </n-alert>
      <n-input :value="plaintext" readonly class="plaintext-field">
        <template #suffix>
          <n-button size="small" @click="onCopy" quaternary >{{ copied ? t('common.copied') : t('common.copy') }}</n-button>
        </template>
      </n-input>
      <!-- Access info at the exact moment the user holds a fresh key: the
           base URL it goes with, and a copy-ready request using this key
           (safe to include — the plaintext is already on screen once-only
           and never stored). -->
      <div class="endpoint-stack">
        <EndpointRow :label="t('apiKeys.endpointOpenAI')" :value="openAIBaseUrl" :pending="endpointPending" />
        <EndpointRow :label="t('apiKeys.endpointExample')" :value="curlWithKey" :pending="endpointPending" wide />
      </div>
    </div>
    <template #footer>
      <n-space justify="end">
        <n-button v-if="step === 'form'" @click="emit('update:show', false)">{{ t('apiKeys.cancel') }}</n-button>
        <n-button v-if="step === 'form'" type="primary" :loading="submitting" @click="onGenerate">{{ t('apiKeys.generateButton') }}</n-button>
        <n-button v-else type="primary" @click="copyAndClose">{{ t('apiKeys.confirmClose') }}</n-button>
      </n-space>
    </template>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NDatePicker, NRadio, NRadioGroup, useDialog, useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useApiKeysStore } from '../../store/apiKeys'
import { useModelsStore } from '../../store/models'
import { useAuthStore } from '../../store/auth'
import { displayMessage } from '../../api/client'
import type { CreateAPIKeyInput } from '../../api/apiKeys'
import { toMicros } from '../../utils/money'
import { modelIdsRule } from '../../utils/apiKeyValidators'
import { copyToClipboard } from '../../utils/clipboard'
import { useGatewayEndpoint } from '../../composables/useGatewayEndpoint'
import EndpointRow from './EndpointRow.vue'
import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import FilterSelectField from '../common/FilterSelectField.vue'
import { useIsMobile } from '../../composables/useIsMobile'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'update:show', v: boolean): void; (e: 'created'): void }>()

// ModalDrawer owns a v-model:show; bridge it to this component's :show /
// @update:show contract. Closing (drawer back arrow, modal ×) routes through
// onUpdateShow so the plaintext step's unsaved-key confirm guard still fires.
const showModel = computed({
  get: () => props.show,
  set: (v) => {
    if (!v) onUpdateShow(false)
  },
})

const { t } = useI18n()
const dialog = useDialog()
const message = useMessage()
const store = useApiKeysStore()
const modelsStore = useModelsStore()
const authStore = useAuthStore()

// Drives the header float position for the expiry picker (mobile drawer vs
// desktop card anchor differently).
const isMobile = useIsMobile()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const step = ref<'form' | 'plaintext'>('form')
const plaintext = ref('')
const copied = ref(false)

// Access info shown on the plaintext step: the gateway base URL this key
// goes with, and a copy-ready example request carrying the fresh key.
const { openAIBaseUrl, curlExample, pending: endpointPending } = useGatewayEndpoint()
const curlWithKey = computed(() => curlExample(plaintext.value))

function initialForm() {
  return {
    remark: '',
    // Default new keys to all-models access; users opt into a specific
    // allowlist via the model-scope toggle.
    allow_all_models: true,
    model_ids: [] as number[],
    expires_at: null as number | null,
    rpm_limit: null as number | null,
    tpm_limit: null as number | null,
    concurrency_limit: null as number | null,
    budget_amount: null as number | null,
  }
}

const form = reactive(initialForm())

// model_ids is required only for a custom allowlist — an all-models key needs
// no selection. Custom-system-prompt is configured post-creation via a
// dedicated modal, so the create form has no CSP field to validate.
const rules = computed<FormRules>(() => ({
  model_ids: modelIdsRule(t, !form.allow_all_models),
}))

const modelOptions = computed(() =>
  modelsStore.list.map((m) => ({ label: m.name, value: m.id })),
)

onMounted(() => {
  // Refresh the model list so the allowlist picker reflects current models.
  // The models store is shared, so this is race-guarded (see store/models.ts).
  // The catalog endpoint is admin-only and the allowlist picker is hidden
  // for members, so they skip the fetch entirely.
  if (authStore.isAdmin) {
    void modelsStore.fetchList().catch((err) => message.error(displayMessage(err, t)))
  }
})

async function onGenerate() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  if (form.expires_at != null && form.expires_at <= Date.now()) {
    message.error(t('apiKeys.expiresMustBeFuture'))
    return
  }
  submitting.value = true
  try {
    // Members may only send label/remark/expiry — the backend rejects any
    // restricted field outright, so the payload must omit them rather than
    // send empty values.
    const input: CreateAPIKeyInput = authStore.isAdmin
      ? {
          remark: form.remark || undefined,
          allow_all_models: form.allow_all_models,
          model_ids: form.model_ids,
          expires_at: form.expires_at != null ? new Date(form.expires_at).toISOString() : undefined,
          rpm_limit: form.rpm_limit ?? undefined,
          tpm_limit: form.tpm_limit ?? undefined,
          concurrency_limit: form.concurrency_limit ?? undefined,
          budget_limit_micros: form.budget_amount != null ? toMicros(form.budget_amount) : undefined,
        }
      : {
          remark: form.remark || undefined,
          expires_at: form.expires_at != null ? new Date(form.expires_at).toISOString() : undefined,
        }
    const res = await store.create(input)
    plaintext.value = res.plaintext_key
    step.value = 'plaintext'
    emit('created')
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    submitting.value = false
  }
}

// Returns whether the clipboard write succeeded so callers that close on copy
// can keep the modal open when it fails (e.g. permission denied, or a
// non-secure HTTP context where navigator.clipboard is undefined) — otherwise
// the one-time key would be lost with no way to retrieve it. The clipboard
// recipe (incl. the execCommand fallback) lives in utils/clipboard.ts, shared
// with the list-page copy button.
async function onCopy(): Promise<boolean> {
  const ok = await copyToClipboard(plaintext.value)
  if (!ok) {
    message.error(t('common.copyFailed'))
    return false
  }
  copied.value = true
  setTimeout(() => {
    copied.value = false
  }, 2000)
  return true
}

// The plaintext step's primary button is labelled "Copy and close": copy the
// one-time key to the clipboard, then close. Copying is itself the save, so
// actually succeeded, so a failed copy leaves the key on screen to retry.
async function copyAndClose() {
  if (await onCopy()) emit('update:show', false)
}

// Dismissing via the card's X (or Esc, though disabled here) without ticking
// unrecoverable afterwards. This path never copies, so its confirm button is
// "close anyway", not the "copy and close" of the primary button.
async function requestClose() {
  if (step.value === 'plaintext') {
    dialog.warning({
      title: t('apiKeys.unsavedConfirmTitle'),
      content: t('apiKeys.unsavedConfirmContent'),
      positiveText: t('apiKeys.closeAnyway'),
      negativeText: t('apiKeys.cancel'),
      onPositiveClick: () => emit('update:show', false),
    })
    return
  }
  emit('update:show', false)
}

function onUpdateShow(v: boolean) {
  // mask-closable=false + close-on-esc=false: the modal only ever emits
  // update:show=false. The v=true branch was unreachable (the parent sets
  // show directly via the prop, not by emitting).
  if (!v) {
    requestClose()
  }
}

function reset() {
  step.value = 'form'
  Object.assign(form, initialForm())
  copied.value = false
  plaintext.value = ''
}
</script>

<style scoped lang="less">
.full-width {
  width: 100%;
}

/* The one-time plaintext step's access info: the endpoint this key goes
   with, plus a copy-ready example request carrying it. Each row is an
   EndpointRow and owns its own layout — only the stacking is set here. */
.endpoint-stack {
  margin-top: var(--space-3);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

/* Group the rate/budget caps under a labelled divider so they read as one
   "limits" block, set apart from the identity and scope fields above. */
.limit-section {
  margin-top: 4px;
  padding-top: 16px;
  border-top: 1px solid var(--n-divider-color, rgba(0, 0, 0, 0.09));
}

.limit-section__label {
  margin-bottom: 8px;
  font-size: 13px;
  color: var(--n-text-color-3, rgba(0, 0, 0, 0.45));
}

/* Each limit holds at most a handful of digits, so a full-width row per field
   stretches the modal needlessly. Lay them two per row; each control fills its
   own cell. */
.limit-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  column-gap: 16px;
}

@media (max-width: @mobile-breakpoint) {
  .limit-grid {
    grid-template-columns: 1fr;
  }
}

.plaintext-step {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.plaintext-warning {
  margin-bottom: var(--space-1);
}
</style>
