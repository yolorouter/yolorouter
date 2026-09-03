<!-- frontend/src/components/models/ModelEditModal.vue -->
<!-- Edits an existing model's public name and image-input declaration,
     prefilled from the `model` prop. Structure (NModal card preset,
     v-model:show, @updated) mirrors ProviderEditModal.vue's
     show/save/emit pattern. -->
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
      <n-form-item path="imageInput">
        <template #label>
          <HelpLabel :tip="t('models.imageInput_tip')">{{ t('models.imageInput') }}</HelpLabel>
        </template>
        <n-select v-model:value="form.imageInput" :options="imageInputOptions" />
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
import { useVideoModalityExclusivity } from '../../composables/useVideoModalityExclusivity'
import type { ImageInputChoice, Model, SchedulingMode } from '../../api/models'
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
  imageInput: ImageInputChoice
  schedulingMode: SchedulingMode
  outputModalities: string[]
}>({
  name: '',
  imageInput: 'unknown',
  schedulingMode: 'failover',
  outputModalities: ['text'],
})
const rules: FormRules = { name: modelNameRule(t) }

const imageInputOptions = computed<SelectOption[]>(() => [
  { label: t('models.imageInputUnknown'), value: 'unknown' },
  { label: t('models.imageInputYes'), value: 'yes' },
  { label: t('models.imageInputNo'), value: 'no' },
])

const schedulingModeOptions = useSchedulingModeOptions()

const outputModalityOptions = computed(() => buildOutputModalityOptions(t))

useVideoModalityExclusivity(form)

function toImageInputChoice(v: boolean | null): ImageInputChoice {
  return v === null ? 'unknown' : v ? 'yes' : 'no'
}

// Order-insensitive comparison: the declaration is a set, and a re-ordered
// list from the multi-select is not a change the server needs to hear about.
function sameModalities(a: string[], b: string[] | undefined): boolean {
  if (!b || a.length !== b.length) return false
  const left = [...a].sort()
  const right = [...b].sort()
  return left.every((v, i) => v === right[i])
}

watch(
  [() => props.show, () => props.model],
  ([visible, model]) => {
    if (!visible || !model) return
    form.name = model.name
    form.imageInput = toImageInputChoice(model.supports_image_input)
    form.schedulingMode = model.scheduling_mode
    form.outputModalities =
      model.output_modalities && model.output_modalities.length > 0 ? [...model.output_modalities] : ['text']
  },
)

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
    const imageChanged = form.imageInput !== toImageInputChoice(props.model.supports_image_input)
    const modeChanged = form.schedulingMode !== props.model.scheduling_mode
    const modalitiesChanged = !sameModalities(form.outputModalities, props.model.output_modalities)
    if (imageChanged || modeChanged || modalitiesChanged) {
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
    // saving an unrelated edit.
    const modeChanged = form.schedulingMode !== props.model.scheduling_mode
    const modalitiesChanged = !sameModalities(form.outputModalities, props.model.output_modalities)
    await store.update(props.model.id, {
      name: form.name,
      imageInput: form.imageInput,
      schedulingMode: modeChanged ? form.schedulingMode : undefined,
      outputModalities: modalitiesChanged ? [...form.outputModalities] : undefined,
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
