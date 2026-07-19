<!-- frontend/src/components/apikeys/EditKeyDrawer.vue
     Edits one key's owner/remark/limits/allowlist via sparse PATCH. Limit
     fields use "clear = unlimited" semantics: an empty input maps to the
     backend's 0-sentinel which clears the column. Expiry can be set or moved
     later but not cleared (backend has no clear-sentinel for timestamps) —
     to remove an expiry, revoke and re-create. -->
<template>
  <n-drawer :show="show" :width="480" @update:show="(v: boolean) => emit('update:show', v)">
    <n-drawer-content :title="t('apiKeys.editTitle')" closable>
      <div v-if="loading" class="loading-row">{{ t('common.loading') }}</div>
      <n-form
        v-else
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
          <NDatePicker v-model:value="form.expires_at" type="datetime" :clearable="false" class="full-width" />
        </n-form-item>
        <n-form-item>
          <template #label>
            <HelpLabel :tip="t('apiKeys.rpmLimit_tip')">{{ t('apiKeys.rpmLimit') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.rpm_limit" :min="0" :placeholder="t('apiKeys.clearByZeroHint')" class="full-width" />
        </n-form-item>
        <n-form-item>
          <template #label>
            <HelpLabel :tip="t('apiKeys.tpmLimit_tip')">{{ t('apiKeys.tpmLimit') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.tpm_limit" :min="0" :placeholder="t('apiKeys.clearByZeroHint')" class="full-width" />
        </n-form-item>
        <n-form-item>
          <template #label>
            <HelpLabel :tip="t('apiKeys.concurrencyLimit_tip')">{{ t('apiKeys.concurrencyLimit') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.concurrency_limit" :min="0" :placeholder="t('apiKeys.clearByZeroHint')" class="full-width" />
        </n-form-item>
        <n-form-item>
          <template #label>
            <HelpLabel :tip="t('apiKeys.budgetLimit_tip')">{{ t('apiKeys.budgetLimit') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.budget_amount" :min="0" :step="0.01" :placeholder="t('apiKeys.clearByZeroHint')" class="full-width" />
        </n-form-item>
      </n-form>

      <template #footer>
        <n-space>
          <n-button @click="emit('update:show', false)">{{ t('apiKeys.cancel') }}</n-button>
          <n-button type="primary" :loading="saving" :disabled="loading" @click="onSave">{{ t('apiKeys.save') }}</n-button>
        </n-space>
      </template>
    </n-drawer-content>
  </n-drawer>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { NDatePicker, useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useApiKeysStore } from '../../store/apiKeys'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { getAPIKey, type APIKey, type UpdateAPIKeyInput } from '../../api/apiKeys'
import { fromCents, toCents } from '../../utils/money'
import HelpLabel from '../HelpLabel.vue'

const props = defineProps<{ show: boolean; apiKeyId: number }>()
const emit = defineEmits<{ (e: 'update:show', v: boolean): void; (e: 'saved'): void }>()

const { t } = useI18n()
const message = useMessage()
const store = useApiKeysStore()
const modelsStore = useModelsStore()

const formRef = ref<FormInst | null>(null)
const loading = ref(true)
const saving = ref(false)

const form = reactive({
  owner_label: '',
  remark: '',
  model_ids: [] as number[],
  expires_at: null as number | null,
  rpm_limit: null as number | null,
  tpm_limit: null as number | null,
  concurrency_limit: null as number | null,
  budget_amount: null as number | null,
})

const rules = computed<FormRules>(() => ({
  owner_label: [{ max: 50, trigger: ['blur', 'input'] }],
  remark: [{ max: 200, trigger: ['blur', 'input'] }],
}))

const modelOptions = computed(() =>
  modelsStore.list.map((m) => ({ label: m.name, value: m.id })),
)

// initialExpiresAt captures the expiry loaded by fill() so onSave can tell
// "user changed expiry" from "user left the original expiry alone". Without
// it, an already-expired key couldn't be edited for unrelated fields (the
// future-time check would reject the loaded past expiry on every save).
const initialExpiresAt = ref<number | null>(null)

function fill(k: APIKey) {
  form.owner_label = k.owner_label
  form.remark = k.remark
  form.model_ids = [...k.model_ids]
  form.expires_at = k.expires_at ? new Date(k.expires_at).getTime() : null
  initialExpiresAt.value = form.expires_at
  form.rpm_limit = k.rpm_limit
  form.tpm_limit = k.tpm_limit
  form.concurrency_limit = k.concurrency_limit
  form.budget_amount = k.budget_limit_cents != null ? fromCents(k.budget_limit_cents) : null
}

onMounted(async () => {
  // The models list is best-effort for the allowlist picker; don't let its
  // failure block loading the key — and fetchList's own .catch already
  // swallowed its rejection, so Promise.all bought nothing but ceremony.
  void modelsStore.fetchList().catch((err) => message.error(displayMessage(err, t)))
  try {
    const key = await getAPIKey(props.apiKeyId)
    fill(key)
  } catch (err) {
    message.error(displayMessage(err, t))
    emit('update:show', false)
  } finally {
    loading.value = false
  }
})

async function onSave() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  // Only validate/send expiry when the user actually changed it. An
  // already-expired key keeps its original expiry (sent as undefined ->
  // backend leaves the column untouched); a fresh future expiry is validated
  // here and forwarded. The picker has :clearable="false" because the backend
  // has no clear-sentinel for timestamps — clearing would silently no-op.
  const expiryChanged = form.expires_at !== initialExpiresAt.value
  if (expiryChanged && form.expires_at != null && form.expires_at <= Date.now()) {
    message.error(t('apiKeys.expiresMustBeFuture'))
    return
  }
  saving.value = true
  try {
    // Numeric limits: empty -> 0 sentinel -> backend clears the column.
    const input: UpdateAPIKeyInput = {
      owner_label: form.owner_label,
      remark: form.remark,
      model_ids: form.model_ids,
      expires_at: expiryChanged && form.expires_at != null ? new Date(form.expires_at).toISOString() : undefined,
      rpm_limit: form.rpm_limit ?? 0,
      tpm_limit: form.tpm_limit ?? 0,
      concurrency_limit: form.concurrency_limit ?? 0,
      budget_limit_cents: form.budget_amount != null ? toCents(form.budget_amount) : 0,
    }
    await store.update(props.apiKeyId, input)
    emit('saved')
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    saving.value = false
  }
}
</script>

<style scoped>
.full-width {
  width: 100%;
}

.loading-row {
  padding: var(--space-4) 0;
  color: var(--color-text-muted);
}
</style>
