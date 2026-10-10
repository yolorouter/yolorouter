<!-- frontend/src/components/models/ModelCapabilityCard.vue -->
<!-- The model detail page's capability summary card: the five admin-declared
     metadata fields (context length, input modalities, tool/reasoning
     tri-states, recommended temperature) plus the candidate-level max-output
     ceiling, at a glance above the route chain.

     Every field is tri-state on the wire; an undeclared field renders as the
     muted "Undeclared" mark — never as a false "no" or a made-up default —
     and a model predating the columns (all null) simply shows every line
     undeclared. The informational note pins the batch's red line: nothing
     here influences routing yet. -->
<template>
  <div class="section-card model-capability-card">
    <div class="section-card__head">{{ t('models.capabilityTitle') }}</div>
    <dl class="model-capability-card__grid">
      <div class="model-capability-card__item" data-field="context-length">
        <dt>
          <HelpLabel :tip="t('models.contextLength_tip')">{{ t('models.contextLength') }}</HelpLabel>
        </dt>
        <dd>
          <span v-if="model.context_length != null" class="model-capability-card__number">{{
            model.context_length.toLocaleString()
          }}</span>
          <span v-else class="model-capability-card__undeclared">{{ t('models.triStateUnknown') }}</span>
        </dd>
      </div>
      <div class="model-capability-card__item" data-field="input-modalities">
        <dt>
          <HelpLabel :tip="t('models.inputModalities_tip')">{{ t('models.inputModalities') }}</HelpLabel>
        </dt>
        <dd>
          <template v-if="declaredInputs.length > 0">
            <n-tag
              v-for="id in declaredInputs"
              :key="id"
              size="small"
              :bordered="false"
              type="info"
              class="model-capability-card__modality"
            >
              {{ inputModalityLabel(id) }}
            </n-tag>
          </template>
          <span v-else class="model-capability-card__undeclared">{{ t('models.triStateUnknown') }}</span>
        </dd>
      </div>
      <div class="model-capability-card__item" data-field="supports-tools">
        <dt>
          <HelpLabel :tip="t('models.supportsTools_tip')">{{ t('models.supportsTools') }}</HelpLabel>
        </dt>
        <dd>
          <n-tag v-if="model.supports_tools === true" size="small" :bordered="false" type="success">
            {{ t('models.triStateYes') }}
          </n-tag>
          <n-tag v-else-if="model.supports_tools === false" size="small" :bordered="false" type="error">
            {{ t('models.triStateNo') }}
          </n-tag>
          <span v-else class="model-capability-card__undeclared">{{ t('models.triStateUnknown') }}</span>
        </dd>
      </div>
      <div class="model-capability-card__item" data-field="supports-reasoning">
        <dt>
          <HelpLabel :tip="t('models.supportsReasoning_tip')">{{ t('models.supportsReasoning') }}</HelpLabel>
        </dt>
        <dd>
          <n-tag v-if="model.supports_reasoning === true" size="small" :bordered="false" type="success">
            {{ t('models.triStateYes') }}
          </n-tag>
          <n-tag v-else-if="model.supports_reasoning === false" size="small" :bordered="false" type="error">
            {{ t('models.triStateNo') }}
          </n-tag>
          <span v-else class="model-capability-card__undeclared">{{ t('models.triStateUnknown') }}</span>
        </dd>
      </div>
      <div class="model-capability-card__item" data-field="temperature">
        <dt>
          <HelpLabel :tip="t('models.temperature_tip')">{{ t('models.temperature') }}</HelpLabel>
        </dt>
        <dd>
          <!-- String(), not toLocaleString: the stored double round-trips
               exactly (a 0.705 declaration must display as 0.705, not a
               rounded 0.71) and a fixed fraction cap would silently re-round
               what the admin typed. -->
          <span v-if="model.temperature != null" class="model-capability-card__number">{{
            String(model.temperature)
          }}</span>
          <span v-else class="model-capability-card__undeclared">{{ t('models.triStateUnknown') }}</span>
        </dd>
      </div>
      <div class="model-capability-card__item" data-field="max-output">
        <dt>
          <HelpLabel :tip="t('models.capabilityMaxOutput_tip')">{{ t('models.maxOutput') }}</HelpLabel>
        </dt>
        <dd>
          <!-- 0 is the candidates' "no extra limit" sentinel, not a zero
               ceiling — surfacing it bare would read as a model that can
               emit nothing. -->
          <span v-if="effectiveMaxOutput === null" class="model-capability-card__undeclared">{{
            t('models.triStateUnknown')
          }}</span>
          <span v-else-if="effectiveMaxOutput === 0">{{ t('models.maxOutputUnlimited') }}</span>
          <span v-else class="model-capability-card__number">{{ effectiveMaxOutput.toLocaleString() }}</span>
        </dd>
      </div>
    </dl>
    <p class="model-capability-card__note">{{ t('models.capabilityInformationalNote') }}</p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import HelpLabel from '../HelpLabel.vue'
import type { Model } from '../../api/models'

const props = defineProps<{ model: Model }>()

const { t } = useI18n()

// A stored declaration is shown as stored; an unknown id (a future modality
// an older console does not know) falls through to the raw id rather than
// being silently dropped.
const INPUT_MODALITY_LABEL_KEYS: Record<string, string> = {
  text: 'models.inputModalityText',
  image: 'models.inputModalityImage',
  audio: 'models.inputModalityAudio',
  video: 'models.inputModalityVideo',
}

function inputModalityLabel(id: string): string {
  const key = INPUT_MODALITY_LABEL_KEYS[id]
  return key ? t(key) : id
}

const declaredInputs = computed(() => props.model.input_modalities ?? [])

// The effective output ceiling this model can emit: the highest max_output
// among the candidates that actually take traffic (enabled ones; a chain
// with none enabled still shows the declared ceiling of its rows). No
// candidates at all = nothing to state → undeclared. The save-time
// consistency check compares exactly this kind of figure against the context
// length, so keeping the two on one card makes the pair readable together.
const effectiveMaxOutput = computed<number | null>(() => {
  const all = props.model.candidates
  if (all.length === 0) return null
  const live = all.filter((c) => c.management_status === 1)
  const pool = live.length > 0 ? live : all
  return Math.max(...pool.map((c) => c.max_output))
})
</script>

<style scoped>
.model-capability-card__grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: var(--space-4) var(--space-6);
  margin: 0;
}

.model-capability-card__item {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.model-capability-card__item dt {
  font-size: var(--text-xs);
  color: var(--color-text-muted);
}

.model-capability-card__item dd {
  margin: 0;
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px;
  min-height: 24px;
}

.model-capability-card__number {
  font-variant-numeric: tabular-nums;
  font-weight: 500;
  color: var(--color-text);
}

.model-capability-card__undeclared {
  font-size: var(--text-xs);
  color: var(--color-text-muted);
}

.model-capability-card__note {
  margin: var(--space-4) 0 0;
  font-size: var(--text-xs);
  color: var(--color-text-muted);
}
</style>
