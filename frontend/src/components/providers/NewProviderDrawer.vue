<!-- frontend/src/components/providers/NewProviderDrawer.vue -->
<template>
  <n-drawer :show="show" width="480" @update:show="onUpdateShow">
    <n-drawer-content :title="t('providers.createButton')" closable>
      <n-steps :current="step" size="small" class="steps">
        <n-step :title="t('providers.name')" />
        <n-step :title="t('providers.keyLabel')" />
      </n-steps>

      <n-form v-if="step === 1" ref="basicFormRef" :model="basicForm" :rules="basicRules" class="form">
        <n-form-item path="name" :label="t('providers.name')">
          <n-input v-model:value="basicForm.name" />
        </n-form-item>
        <n-form-item path="baseUrl" :label="t('providers.baseUrl')">
          <n-input v-model:value="basicForm.baseUrl" placeholder="https://api.example.com/v1" />
        </n-form-item>
        <n-form-item path="note" :label="t('providers.note')">
          <n-input v-model:value="basicForm.note" type="textarea" />
        </n-form-item>
      </n-form>

      <n-form v-else ref="keyFormRef" :model="keyForm" :rules="keyRules" class="form">
        <n-form-item path="label" :label="t('providers.keyLabel')">
          <n-input v-model:value="keyForm.label" />
        </n-form-item>
        <n-form-item path="plaintext" :label="t('providers.keyPlaintext')">
          <n-input v-model:value="keyForm.plaintext" type="password" show-password-on="click" />
        </n-form-item>
        <n-form-item path="testModel" :label="t('providers.testModel')">
          <n-input v-model:value="keyForm.testModel" :placeholder="t('providers.testModelHint')" />
        </n-form-item>
        <n-button :loading="testing" @click="onTestConnection">{{ t('providers.testConnection') }}</n-button>
        <n-alert v-if="testResult" :type="testResult.ok ? 'success' : 'error'" class="test-result">
          {{ testResult.ok ? t('providers.testSuccess') : t('providers.testFailed') }}
        </n-alert>
      </n-form>

      <template #footer>
        <n-space justify="end">
          <n-button @click="onUpdateShow(false)">{{ t('providers.cancel') }}</n-button>
          <n-button v-if="step === 1" type="primary" @click="onNextStep">{{ t('common.save') }}</n-button>
          <n-button v-else type="primary" :loading="submitting" @click="onSubmit">{{ t('providers.save') }}</n-button>
        </n-space>
      </template>
    </n-drawer-content>
  </n-drawer>
</template>

<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useProvidersStore } from '../../store/providers'
import { displayMessage } from '../../api/client'
import { providerNameRule, baseUrlRule, noteRule, keyLabelRule, keyPlaintextRule, testModelRule } from '../../utils/providerValidators'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ 'update:show': [boolean] }>()

const { t } = useI18n()
const message = useMessage()
const store = useProvidersStore()

const step = ref(1)
const basicFormRef = ref<FormInst | null>(null)
const keyFormRef = ref<FormInst | null>(null)
const basicForm = reactive({ name: '', baseUrl: '', note: '' })
const keyForm = reactive({ label: '', plaintext: '', testModel: '' })
const testing = ref(false)
const submitting = ref(false)
const testResult = ref<{ ok: boolean } | null>(null)

// Rule factories live in utils/providerValidators.ts (shared with
// KeyEditDrawer.vue) and mirror the backend's own binding tags
// (createProviderRequest/createKeyRequest in internal/handler/provider_handler.go).
const basicRules: FormRules = {
  name: providerNameRule(t),
  baseUrl: baseUrlRule(t),
  note: noteRule(t),
}
const keyRules: FormRules = {
  label: keyLabelRule(t),
  plaintext: keyPlaintextRule(t, true),
  testModel: testModelRule(t),
}

watch(
  () => props.show,
  (visible) => {
    if (visible) {
      step.value = 1
      basicForm.name = ''
      basicForm.baseUrl = ''
      basicForm.note = ''
      keyForm.label = ''
      keyForm.plaintext = ''
      keyForm.testModel = ''
      testResult.value = null
    }
  },
)

function onUpdateShow(value: boolean) {
  emit('update:show', value)
}

async function onNextStep() {
  try {
    await basicFormRef.value?.validate()
  } catch {
    return
  }
  step.value = 2
}

async function onTestConnection() {
  try {
    await keyFormRef.value?.validate()
  } catch {
    return
  }
  testing.value = true
  testResult.value = null
  try {
    const result = await store.testKeyPreview(basicForm.baseUrl, keyForm.plaintext, keyForm.testModel)
    testResult.value = { ok: result.outcome === 0 }
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    testing.value = false
  }
}

async function onSubmit() {
  try {
    await keyFormRef.value?.validate()
  } catch {
    return
  }
  submitting.value = true
  try {
    await store.create({
      name: basicForm.name,
      base_url: basicForm.baseUrl,
      note: basicForm.note,
      key_label: keyForm.label,
      key_plaintext: keyForm.plaintext,
      test_model: keyForm.testModel,
      management_status: 1, // this drawer's fixed behavior: the first key is always submitted requesting enabled (server independently re-verifies before honoring it, design doc §6)
    })
    onUpdateShow(false)
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.steps {
  margin-bottom: 24px;
}
.form {
  margin-top: 16px;
}
.test-result {
  margin-top: 12px;
}
</style>
