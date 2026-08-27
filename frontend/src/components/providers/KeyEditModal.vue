<!-- frontend/src/components/providers/KeyEditModal.vue -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="editingKey ? t('providers.editKey') : t('providers.addKey')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :confirm-text="t('providers.save')"
    :cancel-text="t('providers.cancel')"
    :loading="submitting"
    :back-label="t('common.back')"
    @confirm="onSubmit"
  >
    <n-form
      require-mark-placement="left"
      ref="formRef"
      :model="form"
      :rules="rules"
      class="provider-form-dense"
      label-placement="left"
      label-align="right"
      label-width="auto"
    >
      <n-form-item path="label">
        <template #label>
          <HelpLabel :tip="t('providers.keyLabel_tip')">{{ t('providers.keyLabel') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.label" />
      </n-form-item>
      <n-form-item path="plaintext">
        <template #label>
          <HelpLabel :tip="t('providers.keyPlaintext_tip')">{{ t('providers.keyPlaintext') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.plaintext" type="password" show-password-on="click"
          :placeholder="plaintextPlaceholder" />
      </n-form-item>
      <ProviderModelTester
        v-model:value="form.testModel"
        :base-url="baseUrl"
        :api-key="form.plaintext"
        :provider-type="providerType"
        :protocol-endpoints="protocolEndpoints"
      />
      <n-form-item>
        <template #label>
          <HelpLabel :tip="t('providers.statusEnabled_tip')">{{ t('providers.statusEnabled') }}</HelpLabel>
        </template>
        <n-switch v-model:value="form.enabled" />
      </n-form-item>
    </n-form>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useProvidersStore } from '../../store/providers'
import { displayMessage } from '../../api/client'
import type { ProviderKey } from '../../api/providers'
import { keyLabelRule, keyPlaintextRule } from '../../utils/providerValidators'
import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import ProviderModelTester from './ProviderModelTester.vue'

const props = defineProps<{
  show: boolean
  providerId: number
  baseUrl: string
  providerType: string
  // How many destinations the server verifies a new plaintext against. Saving
  // waits for that whole walk, so the request budget scales with it.
  destinationCount: number
  // The provider's extra protocol endpoints, as stored. Saving a new
  // plaintext verifies it at every one of them, so the test button covers
  // them too — otherwise it could pass on the primary protocol and the save
  // it precedes still leave the key disabled.
  protocolEndpoints: string
  editingKey?: ProviderKey | null
}>()
// saved carries whether the save re-tested the credential: the server runs a
// fresh test only when a new plaintext was submitted, and callers keeping
// per-key result caches need to know which saves actually produced a new
// verdict.
const emit = defineEmits<{ 'update:show': [boolean]; saved: [retested: boolean] }>()

// ModalDrawer owns a v-model:show; bridge it to this component's existing
// :show / @update:show contract so the parent doesn't have to change.
const showModel = computed({
  get: () => props.show,
  set: (v) => emit('update:show', v),
})

const { t } = useI18n()
const message = useMessage()
const store = useProvidersStore()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const form = reactive({ label: '', plaintext: '', testModel: '', enabled: false })

// computed: the plaintext field is required when adding a brand-new key
// (no prior ciphertext to fall back on) but optional when editing an
// existing one (blank = "keep the current key unchanged").
// Rule factories live in utils/providerValidators.ts (shared with
// NewProviderModal.vue).
const rules = computed<FormRules>(() => ({
  label: keyLabelRule(t),
  plaintext: keyPlaintextRule(t, !props.editingKey),
}))

// The plaintext placeholder previously always
// showed the "please resubmit" hint whenever ANY key was being edited,
// even a healthy, already-passing one — wrongly implying it was broken.
// Only a key that genuinely needs_reentry should show that hint; any
// other edit shows a neutral "leave blank to keep the current key" hint.
const plaintextPlaceholder = computed(() => {
  if (!props.editingKey) return ''
  return props.editingKey.needs_reentry ? t('providers.needsReentry') : t('providers.keepCurrentKeyHint')
})

watch(
  () => props.show,
  (visible) => {
    if (!visible) return
    form.label = props.editingKey?.label ?? ''
    form.plaintext = ''
    form.testModel = props.editingKey?.test_model ?? ''
    form.enabled = props.editingKey?.management_status === 1
  },
)

async function onSubmit() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  submitting.value = true
  try {
    if (props.editingKey) {
      // Only send management_status when the toggle actually changed —
      // this previously always sent an explicit
      // value, defeating the backend's nil-means-unchanged contract for
      // this field (internal/service/provider_service.go's UpdateKeyInput):
      // a pure label/test_model rename on a key that's enabled-but-needs-
      // reentry was being rejected by the enable gate it never asked to
      // touch, because the resent "still enabled" value looked identical
      // to a fresh request to re-enable it.
      const originalEnabled = props.editingKey.management_status === 1
      const managementStatus = form.enabled === originalEnabled ? undefined : (form.enabled ? 1 : 2)
      await store.updateKey(props.providerId, props.editingKey.id, {
        label: form.label,
        plaintext: form.plaintext || undefined,
        test_model: form.testModel,
        management_status: managementStatus,
      }, props.destinationCount)
    } else {
      await store.createKey(props.providerId, {
        label: form.label,
        plaintext: form.plaintext,
        test_model: form.testModel,
        management_status: form.enabled ? 1 : 2,
      }, props.destinationCount)
    }
    emit('saved', !props.editingKey || form.plaintext !== '')
    showModel.value = false
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    submitting.value = false
  }
}
</script>
