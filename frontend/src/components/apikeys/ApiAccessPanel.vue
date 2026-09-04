<!-- frontend/src/components/apikeys/ApiAccessPanel.vue
     The gateway access-info card on the API Keys page: one row per entry
     protocol (OpenAI / Anthropic / Gemini native), each showing the base
     URL API clients of that ecosystem should point at, with a copy button
     and an "examples" button that opens the examples modal straight at that
     protocol's group. Credentials and endpoint only make sense together,
     and this page is where users land once configuration is done. -->
<template>
  <div class="section-card endpoint-panel">
    <span class="endpoint-panel__title">{{ t('apiKeys.endpointTitle') }}</span>
    <EndpointRow
      :label="t('apiKeys.endpointOpenAI')"
      :value="openAIBaseUrl"
      :pending="endpointPending"
      example
      @example="openExamples('openai')"
    />
    <EndpointRow
      :label="t('apiKeys.endpointAnthropic')"
      :value="gatewayEndpoint"
      :pending="endpointPending"
      example
      @example="openExamples('anthropic')"
    />
    <EndpointRow
      :label="t('apiKeys.endpointGemini')"
      :value="geminiBaseUrl"
      :pending="endpointPending"
      example
      @example="openExamples('gemini')"
    />
    <ApiExamplesModal v-model:show="showExamples" :initial-protocol="exampleProtocol" />
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { useGatewayEndpoint } from '../../composables/useGatewayEndpoint'
import type { ExampleProtocol } from '../../utils/apiExamples'
import ApiExamplesModal from './ApiExamplesModal.vue'
import EndpointRow from './EndpointRow.vue'

const { t } = useI18n()

const { endpoint: gatewayEndpoint, openAIBaseUrl, geminiBaseUrl, pending: endpointPending } =
  useGatewayEndpoint()

const showExamples = ref(false)
const exampleProtocol = ref<ExampleProtocol>('openai')

function openExamples(protocol: ExampleProtocol) {
  exampleProtocol.value = protocol
  showExamples.value = true
}
</script>

<style scoped>
/* The frame comes from .section-card; only the tighter padding this
   compact panel wants is overridden here, the way the cost detail pages
   do it. Each row is an EndpointRow. */
.endpoint-panel {
  padding: var(--space-3) var(--space-4);
  margin-bottom: var(--space-4);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.endpoint-panel__title {
  font-size: var(--text-sm);
  font-weight: 600;
  color: var(--color-text-secondary);
}
</style>
