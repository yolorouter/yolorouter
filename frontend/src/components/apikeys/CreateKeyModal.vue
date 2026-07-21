<!-- frontend/src/components/apikeys/CreateKeyModal.vue
     Two-step modal: form -> one-time plaintext reveal. The plaintext is the
     only chance to see the full key (PRD §6.4 KEY-01/04); closing without
     ticking "I have saved it" requires a second confirmation (KEY-03). -->
<template>
  <n-modal
    :show="show"
    preset="card"
    :title="step === 'form' ? t('apiKeys.createTitle') : t('apiKeys.plaintextTitle')"
    style="max-width: 520px"
    :mask-closable="false"
    :close-on-esc="false"
    @update:show="onUpdateShow"
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
      <n-form-item path="owner_label">
        <template #label>
          <HelpLabel :tip="t('apiKeys.ownerLabel_tip')">{{ t('apiKeys.ownerLabel') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.owner_label" :maxlength="50" />
      </n-form-item>
      <n-form-item path="remark">
        <template #label>
          <HelpLabel :tip="t('apiKeys.remark_tip')">{{ t('apiKeys.remark') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.remark" type="textarea" :autosize="{ minRows: 2 }" :maxlength="200" />
      </n-form-item>
      <n-form-item path="model_ids">
        <template #label>
          <HelpLabel :tip="t('apiKeys.modelAllowlist_tip')">{{ t('apiKeys.modelAllowlist') }}</HelpLabel>
        </template>
        <n-select
          v-model:value="form.model_ids"
          multiple
          filterable
          :options="modelOptions"
          :placeholder="t('apiKeys.modelAllowlist')"
        />
      </n-form-item>
      <n-form-item path="expires_at">
        <template #label>
          <HelpLabel :tip="t('apiKeys.expiresAt_tip')">{{ t('apiKeys.expiresAt') }}</HelpLabel>
        </template>
        <NDatePicker v-model:value="form.expires_at" type="datetime" clearable class="full-width" />
      </n-form-item>
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
    </n-form>

    <div v-else class="plaintext-step">
      <n-alert type="warning" :show-icon="true" class="plaintext-warning">
        {{ t('apiKeys.plaintextWarning') }}
      </n-alert>
      <n-input :value="plaintext" readonly class="plaintext-field">
        <template #after>
          <n-button size="small" @click="onCopy">{{ copied ? t('apiKeys.copied') : t('apiKeys.copy') }}</n-button>
        </template>
      </n-input>
      <NCheckbox v-model:checked="saved" class="saved-confirm">
        {{ t('apiKeys.savedConfirm') }}
      </NCheckbox>
    </div>

    <template #footer>
      <n-space justify="end">
        <n-button v-if="step === 'form'" @click="emit('update:show', false)">{{ t('apiKeys.cancel') }}</n-button>
        <n-button v-if="step === 'form'" type="primary" :loading="submitting" @click="onGenerate">{{ t('apiKeys.generateButton') }}</n-button>
        <n-button v-else type="primary" @click="requestClose">{{ t('apiKeys.confirmClose') }}</n-button>
      </n-space>
    </template>
  </n-modal>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NCheckbox, NDatePicker, useDialog, useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useApiKeysStore } from '../../store/apiKeys'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import type { CreateAPIKeyInput } from '../../api/apiKeys'
import { toMicros } from '../../utils/money'
import HelpLabel from '../HelpLabel.vue'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'update:show', v: boolean): void; (e: 'created'): void }>()

const { t } = useI18n()
const dialog = useDialog()
const message = useMessage()
const store = useApiKeysStore()
const modelsStore = useModelsStore()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const step = ref<'form' | 'plaintext'>('form')
const plaintext = ref('')
const saved = ref(false)
const copied = ref(false)

function initialForm() {
  return {
    owner_label: '',
    remark: '',
    model_ids: [] as number[],
    expires_at: null as number | null,
    rpm_limit: null as number | null,
    tpm_limit: null as number | null,
    concurrency_limit: null as number | null,
    budget_amount: null as number | null,
  }
}

const form = reactive(initialForm())

const rules = computed<FormRules>(() => ({
  model_ids: [{ required: true, type: 'array', trigger: ['change', 'blur'], message: t('apiKeys.modelAllowlistRequired') }],
}))

const modelOptions = computed(() =>
  modelsStore.list.map((m) => ({ label: m.name, value: m.id })),
)

onMounted(() => {
  // Refresh the model list so the allowlist picker reflects current models.
  // The models store is shared, so this is race-guarded (see store/models.ts).
  void modelsStore.fetchList().catch((err) => message.error(displayMessage(err, t)))
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
    const input: CreateAPIKeyInput = {
      owner_label: form.owner_label || undefined,
      remark: form.remark || undefined,
      model_ids: form.model_ids,
      expires_at: form.expires_at != null ? new Date(form.expires_at).toISOString() : undefined,
      rpm_limit: form.rpm_limit ?? undefined,
      tpm_limit: form.tpm_limit ?? undefined,
      concurrency_limit: form.concurrency_limit ?? undefined,
      budget_limit_micros: form.budget_amount != null ? toMicros(form.budget_amount) : undefined,
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

async function onCopy() {
  try {
    await navigator.clipboard.writeText(plaintext.value)
    copied.value = true
    setTimeout(() => {
      copied.value = false
    }, 2000)
  } catch {
    message.error(t('apiKeys.copyFailed'))
  }
}

// Closing the plaintext view without ticking "I have saved it" requires a
// second confirmation — the full key is unrecoverable afterwards (KEY-03).
function requestClose() {
  if (step.value === 'plaintext' && !saved.value) {
    dialog.warning({
      title: t('apiKeys.unsavedConfirmTitle'),
      content: t('apiKeys.unsavedConfirmContent'),
      positiveText: t('apiKeys.confirmClose'),
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
  saved.value = false
  copied.value = false
  plaintext.value = ''
}
</script>

<style scoped>
.full-width {
  width: 100%;
}

.plaintext-step {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.plaintext-warning {
  margin-bottom: var(--space-1);
}

.saved-confirm {
  margin-top: var(--space-1);
}
</style>
