<!-- frontend/src/components/models/CandidateEditDrawer.vue -->
<template>
  <n-drawer :show="show" width="480" @update:show="onUpdateShow">
    <n-drawer-content :title="editingCandidate ? t('models.editCandidate') : t('models.addCandidate')" closable>
      <n-form require-mark-placement="left" ref="formRef" :model="form" :rules="rules">
        <n-form-item v-if="!editingCandidate" path="providerId">
          <template #label>
            <HelpLabel :tip="t('models.provider_tip')">{{ t('models.provider') }}</HelpLabel>
          </template>
          <n-select v-model:value="form.providerId" :options="providerOptions" :placeholder="t('models.provider')" />
          <n-button text @click="openNewProviderDrawer">{{ t('providers.createButton') }}</n-button>
        </n-form-item>
        <n-form-item path="providerModelName">
          <template #label>
            <HelpLabel :tip="t('models.providerModelName_tip')">{{ t('models.providerModelName') }}</HelpLabel>
          </template>
          <n-input v-model:value="form.providerModelName" :placeholder="t('models.providerModelNameHint')" />
        </n-form-item>
        <n-form-item path="inputPrice">
          <template #label>
            <HelpLabel :tip="t('models.inputPrice_tip')">{{ t('models.inputPrice') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.inputPrice" :min="0" style="width: 100%" />
        </n-form-item>
        <n-form-item path="outputPrice">
          <template #label>
            <HelpLabel :tip="t('models.outputPrice_tip')">{{ t('models.outputPrice') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.outputPrice" :min="0" style="width: 100%" />
        </n-form-item>
        <n-form-item>
          <template #label>
            <HelpLabel :tip="t('models.cacheWritePrice_tip')">{{ t('models.cacheWritePrice') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.cacheWritePrice" :min="0" style="width: 100%" />
        </n-form-item>
        <n-form-item>
          <template #label>
            <HelpLabel :tip="t('models.cacheReadPrice_tip')">{{ t('models.cacheReadPrice') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.cacheReadPrice" :min="0" style="width: 100%" />
        </n-form-item>
        <n-form-item>
          <template #label>
            <HelpLabel :tip="t('models.maxOutput_tip')">{{ t('models.maxOutput') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.maxOutput" :min="0" style="width: 100%" />
        </n-form-item>
      </n-form>

      <n-space vertical>
        <n-button :loading="testing === 'basic'" @click="onTest('basic')">{{ t('models.testBasic') }}</n-button>
        <n-button :loading="testing === 'streaming'" @click="onTest('streaming')">{{ t('models.testStreaming') }}</n-button>
        <n-button :loading="testing === 'function_calling'" @click="onTest('function_calling')">{{ t('models.testFunctionCalling') }}</n-button>
        <n-alert v-if="testResult" :type="testResult.ok ? 'success' : 'error'">
          {{ testResultLabel }}
        </n-alert>
      </n-space>

      <template #footer>
        <n-space justify="end">
          <n-button @click="onUpdateShow(false)">{{ t('models.cancel') }}</n-button>
          <n-button :loading="submitting" @click="onSave(false)">{{ t('models.saveDisabled') }}</n-button>
          <n-button type="primary" :disabled="!basicTestPassed" :loading="submitting" @click="onSave(true)">
            {{ t('models.saveEnabled') }}
          </n-button>
        </n-space>
      </template>
    </n-drawer-content>
  </n-drawer>

  <NewProviderDrawer v-model:show="showNewProviderDrawer" />
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage, type FormInst, type FormRules } from 'naive-ui'
import { useModelsStore } from '../../store/models'
import { useProvidersStore } from '../../store/providers'
import { displayMessage } from '../../api/client'
import { providerModelNameRule, nonNegativePriceRule } from '../../utils/modelValidators'
import { testOutcomeI18nKey } from '../../utils/testOutcomeDisplay'
import HelpLabel from '../HelpLabel.vue'
import NewProviderDrawer from '../providers/NewProviderDrawer.vue'
import type { ModelCandidate } from '../../api/models'

const props = defineProps<{ show: boolean; modelId: number; editingCandidate?: ModelCandidate | null }>()
const emit = defineEmits<{ 'update:show': [boolean]; saved: [] }>()

const { t } = useI18n()
const message = useMessage()
const store = useModelsStore()
const providersStore = useProvidersStore()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const testing = ref<'basic' | 'streaming' | 'function_calling' | null>(null)
// outcome is only carried by the new-mapping test (testMapping returns a
// TestOutcome int); the editing-candidate branch tests booleans (streaming /
// function_calling / verification_status) and leaves it undefined.
const testResult = ref<{ ok: boolean; outcome?: number } | null>(null)
// basicTestPassed only gates the "save and enable" button in the UI for the
// new-candidate flow — the server independently re-runs the basic test on
// its own before honoring an enabled create request (design doc §5), so this
// is a UX nicety, not the actual enforcement point.
const basicTestPassed = computed(() => !!props.editingCandidate || testResult.value?.ok === true)

// Result alert text: on a failed new-mapping test, append the specific
// outcome reason (via the shared testOutcomeI18nKey, same as the provider
// surfaces) so a wrong model/bad key/unreachable address is distinguishable
// rather than a blanket "test failed".
const testResultLabel = computed(() => {
  const r = testResult.value
  if (!r) return ''
  if (r.ok) return t('models.testPassed')
  if (r.outcome !== undefined) {
    return `${t('models.testFailed')}: ${t(`providers.${testOutcomeI18nKey(r.outcome)}`)}`
  }
  return t('models.testFailed')
})

const showNewProviderDrawer = ref(false)
let providerIdBeforeCreate = 0

const form = reactive({
  providerId: null as number | null,
  providerModelName: '',
  inputPrice: 0,
  outputPrice: 0,
  cacheWritePrice: undefined as number | undefined,
  cacheReadPrice: undefined as number | undefined,
  maxOutput: 0,
})

const providerOptions = computed(() =>
  providersStore.list.map((p) => ({ label: p.name, value: p.id, disabled: false })),
)

const rules: FormRules = {
  providerId: [{ required: true, type: 'number', message: t('models.fieldRequired'), trigger: ['change', 'blur'] }],
  providerModelName: providerModelNameRule(t),
  inputPrice: nonNegativePriceRule(t),
  outputPrice: nonNegativePriceRule(t),
}

watch(
  () => props.show,
  (visible) => {
    if (!visible) return
    testResult.value = null
    if (props.editingCandidate) {
      form.providerId = props.editingCandidate.provider_id
      form.providerModelName = props.editingCandidate.provider_model_name
      form.inputPrice = props.editingCandidate.input_price
      form.outputPrice = props.editingCandidate.output_price
      form.cacheWritePrice = props.editingCandidate.cache_write_price ?? undefined
      form.cacheReadPrice = props.editingCandidate.cache_read_price ?? undefined
      form.maxOutput = props.editingCandidate.max_output
    } else {
      form.providerId = null
      form.providerModelName = ''
      form.inputPrice = 0
      form.outputPrice = 0
      form.cacheWritePrice = undefined
      form.cacheReadPrice = undefined
      form.maxOutput = 0
      providersStore.fetchList()
    }
  },
)

function onUpdateShow(value: boolean) {
  emit('update:show', value)
}

function openNewProviderDrawer() {
  // NewProviderDrawer.vue only emits 'update:show' (M2's /simplify pass
  // removed an unused 'created' emit) — so instead of listening for a
  // creation event, capture the highest existing provider id, then diff
  // against the refetched list once the drawer closes.
  providerIdBeforeCreate = providersStore.list.reduce((max, p) => Math.max(max, p.id), 0)
  showNewProviderDrawer.value = true
}

watch(showNewProviderDrawer, async (visible) => {
  if (visible) return
  await providersStore.fetchList()
  const created = providersStore.list.find((p) => p.id > providerIdBeforeCreate)
  if (created) form.providerId = created.id
})

async function onTest(testType: 'basic' | 'streaming' | 'function_calling') {
  // providerModelName is optional — a blank value defaults to the model's
  // own name server-side (see modelValidators.ts's providerModelNameRule).
  if (!form.providerId) {
    message.error(t('models.fieldRequired'))
    return
  }
  testing.value = testType
  try {
    if (props.editingCandidate) {
      const result = await store.testCandidate(props.modelId, props.editingCandidate.id, testType)
      testResult.value = { ok: testType === 'basic' ? result.verification_status === 1 : (testType === 'streaming' ? result.supports_streaming : result.supports_function_calling) }
    } else {
      const result = await store.testMapping(props.modelId, form.providerId, form.providerModelName, testType)
      testResult.value = { ok: result.outcome === 0, outcome: result.outcome }
    }
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    testing.value = null
  }
}

async function onSave(enable: boolean) {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  submitting.value = true
  try {
    if (props.editingCandidate) {
      await store.updateCandidate(props.modelId, props.editingCandidate.id, {
        provider_model_name: form.providerModelName,
        input_price: form.inputPrice,
        output_price: form.outputPrice,
        cache_write_price: form.cacheWritePrice,
        cache_read_price: form.cacheReadPrice,
        max_output: form.maxOutput,
      })
    } else {
      await store.createCandidate(props.modelId, {
        provider_id: form.providerId!,
        provider_model_name: form.providerModelName,
        input_price: form.inputPrice,
        output_price: form.outputPrice,
        cache_write_price: form.cacheWritePrice,
        cache_read_price: form.cacheReadPrice,
        max_output: form.maxOutput,
        management_status: enable ? 1 : 2,
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
