<!-- frontend/src/components/providers/NewProviderDrawer.vue -->
<template>
  <n-drawer :show="show" width="480" @update:show="onUpdateShow">
    <n-drawer-content :title="t('providers.createButton')" closable>
      <n-steps :current="step" size="small" class="steps">
        <n-step :title="t('providers.name')" />
        <n-step :title="t('providers.keyLabel')" />
      </n-steps>

      <n-form require-mark-placement="left" v-if="step === 1" ref="basicFormRef" :model="basicForm" :rules="basicRules" class="form">
        <n-form-item path="name">
          <template #label>
            <HelpLabel :tip="t('providers.name_tip')">{{ t('providers.name') }}</HelpLabel>
          </template>
          <n-input v-model:value="basicForm.name" />
        </n-form-item>
        <n-form-item path="baseUrl">
          <template #label>
            <HelpLabel :tip="t('providers.baseUrl_tip')">{{ t('providers.baseUrl') }}</HelpLabel>
          </template>
          <n-input v-model:value="basicForm.baseUrl" placeholder="https://api.example.com/v1" />
        </n-form-item>
        <n-form-item path="note">
          <template #label>
            <HelpLabel :tip="t('providers.note_tip')">{{ t('providers.note') }}</HelpLabel>
          </template>
          <n-input v-model:value="basicForm.note" type="textarea" />
        </n-form-item>
      </n-form>

      <n-form require-mark-placement="left" v-else ref="keyFormRef" :model="keyForm" :rules="keyRules" class="form">
        <n-form-item path="label">
          <template #label>
            <HelpLabel :tip="t('providers.keyLabel_tip')">{{ t('providers.keyLabel') }}</HelpLabel>
          </template>
          <n-input v-model:value="keyForm.label" />
        </n-form-item>
        <n-form-item path="plaintext">
          <template #label>
            <HelpLabel :tip="t('providers.keyPlaintext_tip')">{{ t('providers.keyPlaintext') }}</HelpLabel>
          </template>
          <n-input v-model:value="keyForm.plaintext" type="password" show-password-on="click" />
        </n-form-item>
        <n-form-item path="testModel">
          <template #label>
            <HelpLabel :tip="t('providers.testModel_tip')">{{ t('providers.testModel') }}</HelpLabel>
          </template>
          <n-input v-model:value="keyForm.testModel" :placeholder="t('providers.testModelHint')" />
        </n-form-item>
        <n-button :loading="testing" @click="onTestConnection">{{ t('providers.testConnection') }}</n-button>
        <n-alert v-if="testOutcome !== null" :type="testResultOk ? 'success' : 'error'" class="test-result">
          {{ testResultText }}
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
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useProvidersStore } from '../../store/providers'
import { displayMessage } from '../../api/client'
import HelpLabel from '../HelpLabel.vue'
import { providerNameRule, baseUrlRule, noteRule, keyLabelRule, keyPlaintextRule, testModelRule } from '../../utils/providerValidators'
import { testOutcomeI18nKey } from '../../utils/testOutcomeDisplay'

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
// Holds the last test's outcome int (backend service.TestOutcome enum); null
// until a test runs. Success (outcome 0) vs the specific failure reason are
// both derived from it — see testResultOk / testResultText.
const testOutcome = ref<number | null>(null)

const testResultOk = computed(() => testOutcome.value === 0)

// Failure line shown in the result alert: the specific reason (e.g.
// "Unreachable") instead of a blanket "Test failed", so an admin can tell a
// bad key from a wrong model from a blocked/unreachable address at a glance.
const testResultText = computed(() => {
  if (testOutcome.value === null) return ''
  if (testResultOk.value) return t('providers.testSuccess')
  return `${t('providers.testFailed')}: ${t(`providers.${testOutcomeI18nKey(testOutcome.value)}`)}`
})

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
      testOutcome.value = null
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
  testOutcome.value = null
  try {
    const result = await store.testKeyPreview(basicForm.baseUrl, keyForm.plaintext, keyForm.testModel)
    testOutcome.value = result.outcome
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
      management_status: 1, // this drawer's fixed behavior: the first key is always submitted requesting enabled (server independently re-verifies before honoring it)
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
