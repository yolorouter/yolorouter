<!-- frontend/src/components/models/ModelEditModal.vue -->
<!-- Edits an existing model's public name and its capability declarations
     (input/output modalities, context length, tool/reasoning tri-states,
     recommended temperature), prefilled from the `model` prop. Structure
     (NModal card preset, v-model:show, @updated) mirrors ProviderEditModal.vue's
     show/save/emit pattern.

     Save protocol: the update endpoint reads an absent field as "keep", so
     every capability field rides along ONLY when the form differs from the
     row it was prefilled from — an untouched field stays absent, and a
     tri-state put back to its original choice is indistinguishable (and
     correctly so) from one never touched. The input-modality multi-select
     supersedes the old image-input boolean select: the server derives the
     deprecated column from every input_modalities write. -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="t('models.editModel')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :confirm-text="t('models.save')"
    :cancel-text="t('models.cancel')"
    :loading="submitting"
    :back-label="t('common.back')"
    @confirm="onSubmit"
  >
    <n-form require-mark-placement="left" ref="formRef" :model="form" :rules="rules">
      <n-form-item path="name">
        <template #label>
          <HelpLabel :tip="t('models.name_tip')">{{ t('models.name') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.name" :placeholder="t('models.nameHint')" />
      </n-form-item>
      <n-form-item path="inputModalities">
        <template #label>
          <HelpLabel :tip="t('models.inputModalities_tip')">{{ t('models.inputModalities') }}</HelpLabel>
        </template>
        <!-- Unlike the output list, an EMPTY selection is legal and means
             "undeclared": an input declaration has a meaningful no-declaration
             state (the tri-state the old image-input boolean carried). -->
        <n-select
          v-model:value="form.inputModalities"
          multiple
          clearable
          class="model-edit__input-modalities"
          :options="inputModalityOptions"
          :placeholder="t('models.undeclaredPlaceholder')"
        />
      </n-form-item>
      <n-form-item path="schedulingMode">
        <template #label>
          <HelpLabel :tip="t('models.schedulingMode_tip')">{{ t('models.schedulingMode') }}</HelpLabel>
        </template>
        <n-select v-model:value="form.schedulingMode" :options="schedulingModeOptions" />
      </n-form-item>
      <n-form-item path="outputModalities">
        <template #label>
          <HelpLabel :tip="t('models.outputModalities_tip')">{{ t('models.outputModalities') }}</HelpLabel>
        </template>
        <!-- Multiple with a non-empty starting value: a model always produces
             something, and clearing the last entry would leave nothing for the
             endpoints to match against. -->
        <n-select v-model:value="form.outputModalities" multiple :options="outputModalityOptions" />
      </n-form-item>
      <n-form-item path="contextLength">
        <template #label>
          <HelpLabel :tip="t('models.contextLength_tip')">{{ t('models.contextLength') }}</HelpLabel>
        </template>
        <div class="model-edit__context">
          <!-- Clearable + null value IS the undeclared state — there is no
               separate "unknown" option to pick. -->
          <n-input-number
            v-model:value="form.contextLength"
            class="model-edit__context-input"
            :min="1"
            :precision="0"
            :placeholder="t('models.undeclaredPlaceholder')"
          />
          <div class="model-edit__context-presets">
            <n-button
              v-for="preset in CONTEXT_LENGTH_PRESETS"
              :key="preset.value"
              size="tiny"
              quaternary
              type="primary"
              @click="form.contextLength = preset.value"
            >
              {{ preset.label }}
            </n-button>
          </div>
        </div>
      </n-form-item>
      <n-form-item path="supportsTools">
        <template #label>
          <HelpLabel :tip="t('models.supportsTools_tip')">{{ t('models.supportsTools') }}</HelpLabel>
        </template>
        <n-select v-model:value="form.supportsTools" class="model-edit__tools" :options="triStateOptions" />
      </n-form-item>
      <n-form-item path="supportsReasoning">
        <template #label>
          <HelpLabel :tip="t('models.supportsReasoning_tip')">{{ t('models.supportsReasoning') }}</HelpLabel>
        </template>
        <n-select v-model:value="form.supportsReasoning" class="model-edit__reasoning" :options="triStateOptions" />
      </n-form-item>
      <n-form-item path="temperature">
        <template #label>
          <HelpLabel :tip="t('models.temperature_tip')">{{ t('models.temperature') }}</HelpLabel>
        </template>
        <!-- Same [0,2] closed range the playground's parameter panel binds;
             cleared input = undeclared. -->
        <n-input-number
          v-model:value="form.temperature"
          class="model-edit__temperature"
          :min="0"
          :max="2"
          :step="0.1"
          :placeholder="t('models.undeclaredPlaceholder')"
        />
      </n-form-item>
    </n-form>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NSelect, useDialog, useMessage, type FormInst, type FormRules, type SelectOption } from 'naive-ui'
import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { useSchedulingModeOptions } from '../../utils/schedulingMode'
import { outputModalityOptions as buildOutputModalityOptions } from '../../utils/modalityOptions'
import { useModalityExclusivity } from '../../composables/useModalityExclusivity'
// The context-length quick picks — shared with the bulk import dialog's
// detail pane so the two can never drift (see utils/contextLengthPresets).
import { CONTEXT_LENGTH_PRESETS } from '../../utils/contextLengthPresets'
import type { Model, SchedulingMode, TriStateChoice } from '../../api/models'
import { modelNameRule } from '../../utils/modelValidators'
import { modelRenameContent } from '../../utils/impactSummary'

const props = defineProps<{ show: boolean; model: Model | null }>()
const emit = defineEmits<{ 'update:show': [boolean]; updated: [] }>()

// ModalDrawer owns a v-model:show; bridge it to this component's existing
// :show / @update:show contract so the parent doesn't have to change.
const showModel = computed({
  get: () => props.show,
  set: (v) => emit('update:show', v),
})

const { t } = useI18n()
const dialog = useDialog()
const message = useMessage()
const store = useModelsStore()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
const form = reactive<{
  name: string
  inputModalities: string[]
  schedulingMode: SchedulingMode
  outputModalities: string[]
  contextLength: number | null
  supportsTools: TriStateChoice
  supportsReasoning: TriStateChoice
  temperature: number | null
}>({
  name: '',
  inputModalities: [],
  schedulingMode: 'failover',
  outputModalities: ['text'],
  contextLength: null,
  supportsTools: 'unknown',
  supportsReasoning: 'unknown',
  temperature: null,
})
const rules: FormRules = { name: modelNameRule(t) }

// One tri-state option set for the tool/reasoning selects — the same
// yes/no/unknown shape the image-input select used to carry alone.
const triStateOptions = computed<SelectOption[]>(() => [
  { label: t('models.triStateUnknown'), value: 'unknown' },
  { label: t('models.triStateYes'), value: 'yes' },
  { label: t('models.triStateNo'), value: 'no' },
])

const inputModalityOptions = computed<SelectOption[]>(() => [
  { label: t('models.inputModalityText'), value: 'text' },
  { label: t('models.inputModalityImage'), value: 'image' },
  { label: t('models.inputModalityAudio'), value: 'audio' },
  { label: t('models.inputModalityVideo'), value: 'video' },
])

const schedulingModeOptions = useSchedulingModeOptions()

const outputModalityOptions = computed(() => buildOutputModalityOptions(t))

useModalityExclusivity(form)

function toTriState(v: boolean | null): TriStateChoice {
  return v === null ? 'unknown' : v ? 'yes' : 'no'
}

// Order-insensitive comparison: the declaration is a set, and a re-ordered
// list from the multi-select is not a change the server needs to hear about.
// An undefined/null stored list reads as the empty selection — undeclared.
function sameModalities(a: string[], b: string[] | null | undefined): boolean {
  const right = b ?? []
  if (a.length !== right.length) return false
  const left = [...a].sort()
  const sorted = [...right].sort()
  return left.every((v, i) => v === sorted[i])
}

watch(
  [() => props.show, () => props.model],
  ([visible, model]) => {
    if (!visible || !model) return
    form.name = model.name
    form.inputModalities = model.input_modalities ? [...model.input_modalities] : []
    form.schedulingMode = model.scheduling_mode
    form.outputModalities =
      model.output_modalities && model.output_modalities.length > 0 ? [...model.output_modalities] : ['text']
    form.contextLength = model.context_length ?? null
    form.supportsTools = toTriState(model.supports_tools)
    form.supportsReasoning = toTriState(model.supports_reasoning)
    form.temperature = model.temperature ?? null
  },
)

// Which capability fields the form moved away from the prefilled row. Each
// changed field rides the PATCH; each unchanged one stays absent so the
// server keeps its stored value (the absent=keep contract on the wire).
function capabilityChanges() {
  const model = props.model!
  return {
    inputModalities: sameModalities(form.inputModalities, model.input_modalities)
      ? undefined
      : [...form.inputModalities],
    contextLength: form.contextLength === (model.context_length ?? null) ? undefined : form.contextLength,
    supportsTools: form.supportsTools === toTriState(model.supports_tools) ? undefined : form.supportsTools,
    supportsReasoning:
      form.supportsReasoning === toTriState(model.supports_reasoning) ? undefined : form.supportsReasoning,
    temperature: form.temperature === (model.temperature ?? null) ? undefined : form.temperature,
  }
}

async function onSubmit() {
  if (!props.model) return
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  // An unchanged name is not a rename. If nothing else changed either, close
  // without a request; the rename confirm (live-traffic warning) exists for
  // renames alone and must not scare a declaration or scheduling edit.
  if (form.name === props.model.name) {
    const modeChanged = form.schedulingMode !== props.model.scheduling_mode
    const outputChanged = !sameModalities(form.outputModalities, props.model.output_modalities)
    const capabilities = capabilityChanges()
    const capabilityChanged = Object.values(capabilities).some((v) => v !== undefined)
    if (modeChanged || outputChanged || capabilityChanged) {
      void doSave()
      return
    }
    showModel.value = false
    return
  }
  // Renaming breaks callers, not key allowlists (those follow the model id),
  // so the confirm leads with the live-traffic number for the old name.
  // submitting covers the fetch too: a second click while the impact loads
  // would stack a second confirm dialog.
  submitting.value = true
  let content: string
  try {
    content = await modelRenameContent(props.model.id, props.model.name, t)
  } finally {
    submitting.value = false
  }
  // The cancel button stays live while the preview loads, so the editor may
  // already be closed by the time it arrives. A confirm dialog for a rename
  // the user walked away from must not appear — accepting it would rename
  // anyway.
  if (!props.show || !props.model) return
  dialog.warning({
    title: t('models.confirmRenameModelTitle'),
    content,
    style: 'white-space: pre-line',
    positiveText: t('models.save'),
    negativeText: t('models.cancel'),
    onPositiveClick: () => {
      void doSave()
    },
  })
}

async function doSave() {
  if (!props.model) return
  submitting.value = true
  try {
    // The scheduling mode rides along only when THIS dialog changed it: the
    // update endpoint reads an absent field as "keep", so a form opened
    // before another admin switched the mode cannot switch it back merely by
    // saving an unrelated edit. The capability fields follow the same rule
    // via capabilityChanges().
    const modeChanged = form.schedulingMode !== props.model.scheduling_mode
    const outputChanged = !sameModalities(form.outputModalities, props.model.output_modalities)
    await store.update(props.model.id, {
      name: form.name,
      schedulingMode: modeChanged ? form.schedulingMode : undefined,
      outputModalities: outputChanged ? [...form.outputModalities] : undefined,
      ...capabilityChanges(),
    })
    message.success(t('models.saveSuccess'))
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
.model-edit__context {
  display: flex;
  flex-direction: column;
  gap: 6px;
  width: 100%;
}

.model-edit__context-presets {
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
}
</style>
