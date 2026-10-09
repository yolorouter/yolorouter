<!-- frontend/src/components/providers/ProviderEditModal.vue -->
<!-- Mirrors NewProviderModal.vue's step-1 form (name/baseUrl/note +
     ProtocolConfigFields) but with no key step — this modal only edits an
     EXISTING provider's basic info and protocol config, prefilled from the
     `provider` prop. Structure (NModal card preset, v-model:show,
     @updated) mirrors KeyEditModal.vue's own show/save/emit pattern. -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="t('providers.editProvider')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :confirm-text="t('providers.save')"
    :cancel-text="t('providers.cancel')"
    :loading="submitting"
    :back-label="t('common.back')"
    @confirm="onSubmit"
  >
    <n-alert type="warning" class="reverify-warning">
      {{ t('providers.editProtocolReverifyWarning') }}
    </n-alert>
    <n-form
      require-mark-placement="left"
      ref="formRef"
      :model="form"
      :rules="rules"
      class="form provider-form-dense"
      label-placement="left"
      label-align="right"
      label-width="auto"
    >
      <n-form-item path="name">
        <template #label>
          <HelpLabel :tip="t('providers.name_tip')">{{ t('providers.name') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.name" />
      </n-form-item>
      <n-form-item path="baseUrl">
        <template #label>
          <HelpLabel :tip="t('providers.baseUrl_tip')">{{ t('providers.baseUrl') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.baseUrl" placeholder="https://api.example.com/v1" />
      </n-form-item>
      <ProtocolConfigFields v-model="form.protocol" />
      <!-- Outbound-proxy override: tri-state select (inherit / force
           direct / custom) plus the custom-mode proxy URL. Same
           visible-but-disabled treatment as the settings card: a stored URL
           stays readable outside custom mode instead of vanishing. -->
      <n-form-item path="proxyMode">
        <template #label>
          <HelpLabel :tip="t('providers.proxyMode_tip')">{{ t('providers.proxyMode') }}</HelpLabel>
        </template>
        <n-select v-model:value="form.proxyMode" class="provider-proxy__mode" :options="proxyModeOptions" />
      </n-form-item>
      <n-form-item path="proxyUrl">
        <template #label>
          <HelpLabel :tip="t('providers.proxyUrl_tip')">{{ t('providers.proxyUrl') }}</HelpLabel>
        </template>
        <n-input
          v-model:value="form.proxyUrl"
          class="provider-proxy__url"
          :placeholder="t('providers.proxyUrlPlaceholder')"
          :disabled="form.proxyMode !== 'custom'"
        />
      </n-form-item>
      <n-form-item path="note">
        <template #label>
          <HelpLabel :tip="t('providers.note_tip')">{{ t('providers.note') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.note" type="textarea" />
      </n-form-item>
    </n-form>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage, type FormInst, type FormRules, type SelectOption } from 'naive-ui'
import { useProvidersStore } from '../../store/providers'
import { displayMessage } from '../../api/client'
import type { Provider } from '../../api/providers'
import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import ProtocolConfigFields from './ProtocolConfigFields.vue'
import { providerNameRule, baseUrlRule, noteRule } from '../../utils/providerValidators'
import {
  emptyProtocolConfig,
  parseProtocolConfig,
  protocolEndpointsValid,
  serializeProtocolConfig,
  type ProtocolConfigModel,
} from '../../utils/providerProtocol'
import {
  checkProxyUrl,
  providerProxySavePlan,
  proxyUrlIssueMessageKey,
} from '../../utils/outboundProxy'

const props = defineProps<{ show: boolean; provider: Provider | null }>()
const emit = defineEmits<{ 'update:show': [boolean]; updated: [] }>()

// ModalDrawer owns a v-model:show; bridge it to this component's existing
// :show / @update:show contract so parents don't have to change.
const showModel = computed({
  get: () => props.show,
  set: (v) => emit('update:show', v),
})

const { t } = useI18n()
const message = useMessage()
const store = useProvidersStore()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const form = reactive({
  name: '',
  baseUrl: '',
  note: '',
  proxyMode: '',
  proxyUrl: '',
  protocol: emptyProtocolConfig('openai') as ProtocolConfigModel,
})

const proxyModeOptions = computed<SelectOption[]>(() => [
  { value: '', label: t('providers.proxyModeInherit') },
  { value: 'off', label: t('providers.proxyModeOff') },
  { value: 'custom', label: t('providers.proxyModeCustom') },
])

// Rule factories live in utils/providerValidators.ts (shared with
// NewProviderModal.vue / KeyEditModal.vue).
const rules: FormRules = {
  name: providerNameRule(t),
  baseUrl: baseUrlRule(t),
  note: noteRule(t),
  // Required only under custom mode; the server re-validates and also
  // refuses reserved-range addresses the browser cannot judge.
  proxyUrl: {
    required: true,
    validator: (_rule, value: string) => form.proxyMode !== 'custom' || checkProxyUrl(value).ok,
    message: () => {
      const check = checkProxyUrl(form.proxyUrl)
      return check.ok ? '' : t(proxyUrlIssueMessageKey('provider', check.issue))
    },
    trigger: ['blur'],
  },
}

watch(
  [() => props.show, () => props.provider],
  ([visible, provider]) => {
    if (!visible || !provider) return
    form.name = provider.name
    form.baseUrl = provider.base_url
    form.note = provider.note
    form.proxyMode = provider.proxy_mode ?? ''
    form.proxyUrl = provider.proxy_url ?? ''
    form.protocol = parseProtocolConfig(provider.provider_type, provider.protocol_endpoints)
  },
)

async function onSubmit() {
  if (!props.provider) return
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  if (!protocolEndpointsValid(form.protocol)) {
    message.error(t('providers.protocolEndpointUrlInvalid'))
    return
  }
  const proxyPlan = providerProxySavePlan(form.proxyMode, form.proxyUrl)
  if (!proxyPlan.ok) {
    message.error(t(proxyUrlIssueMessageKey('provider', proxyPlan.issue)))
    return
  }
  submitting.value = true
  try {
    await store.update(props.provider.id, {
      name: form.name,
      base_url: form.baseUrl,
      note: form.note,
      ...serializeProtocolConfig(form.protocol),
      ...proxyPlan.payload,
    })
    message.success(t('providers.saveSuccess'))
    emit('updated')
    showModel.value = false
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.reverify-warning {
  margin-bottom: 16px;
}
.form {
  margin-top: 0;
}
</style>
