<!-- frontend/src/components/providers/KeyEditDrawer.vue -->
<template>
  <n-drawer :show="show" width="420" @update:show="onUpdateShow">
    <n-drawer-content :title="editingKey ? t('providers.editKey') : t('providers.addKey')" closable>
      <n-form require-mark-placement="left" ref="formRef" :model="form" :rules="rules">
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
        <n-form-item path="testModel">
          <template #label>
            <HelpLabel :tip="t('providers.testModel_tip')">{{ t('providers.testModel') }}</HelpLabel>
          </template>
          <n-input v-model:value="form.testModel" :placeholder="t('providers.testModelHint')" />
        </n-form-item>
        <n-form-item>
          <template #label>
            <HelpLabel :tip="t('providers.statusEnabled_tip')">{{ t('providers.statusEnabled') }}</HelpLabel>
          </template>
          <n-switch v-model:value="form.enabled" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="onUpdateShow(false)">{{ t('providers.cancel') }}</n-button>
          <n-button type="primary" :loading="submitting" @click="onSubmit">{{ t('providers.save') }}</n-button>
        </n-space>
      </template>
    </n-drawer-content>
  </n-drawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useProvidersStore } from '../../store/providers'
import { displayMessage } from '../../api/client'
import type { ProviderKey } from '../../api/providers'
import { keyLabelRule, keyPlaintextRule, testModelRule } from '../../utils/providerValidators'
import HelpLabel from '../HelpLabel.vue'

const props = defineProps<{ show: boolean; providerId: number; editingKey?: ProviderKey | null }>()
const emit = defineEmits<{ 'update:show': [boolean]; saved: [] }>()

const { t } = useI18n()
const message = useMessage()
const store = useProvidersStore()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const form = reactive({ label: '', plaintext: '', testModel: '', enabled: false })

// computed: the plaintext field is required when adding a brand-new key
// (no prior ciphertext to fall back on) but optional when editing an
// existing one (blank = "keep the current key unchanged", design doc §8).
// Rule factories live in utils/providerValidators.ts (shared with
// NewProviderDrawer.vue).
const rules = computed<FormRules>(() => ({
  label: keyLabelRule(t),
  plaintext: keyPlaintextRule(t, !props.editingKey),
  testModel: testModelRule(t),
}))

// A max-effort code-review round found the plaintext placeholder always
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

function onUpdateShow(value: boolean) {
  emit('update:show', value)
}

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
      // a max-effort code-review round found this always sent an explicit
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
      })
    } else {
      await store.createKey(props.providerId, {
        label: form.label,
        plaintext: form.plaintext,
        test_model: form.testModel,
        management_status: form.enabled ? 1 : 2,
      })
    }
    emit('saved')
    onUpdateShow(false)
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    submitting.value = false
  }
}
</script>
