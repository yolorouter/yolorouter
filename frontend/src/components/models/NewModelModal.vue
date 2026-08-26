<!-- frontend/src/components/models/NewModelModal.vue -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="t('models.createButton')"
    max-width="560px"
    :mask-closable="false"
    :close-on-esc="false"
    :back-label="t('common.back')"
  >
    <!-- Preset catalogue: pick common model IDs by vendor and batch-add them.
         Already-existing names are shown disabled with an "added" badge; the
         server skips them anyway, this just makes the diff obvious. -->
    <div class="preset-section">
      <HelpLabel :tip="t('models.presetHint')" class="preset-label">{{ t('models.presetTitle') }}</HelpLabel>
      <div class="preset-groups">
        <div v-for="g in groups" :key="g.vendorId" class="preset-group">
          <NCheckbox
            :checked="groupChecked(g)"
            :indeterminate="groupIndeterminate(g)"
            :disabled="groupSelectable(g).length === 0"
            @update:checked="(v: boolean) => toggleGroup(g, v)"
          >
            <span class="preset-group__name">{{ g.name }}</span>
          </NCheckbox>
          <div class="preset-group__models">
            <NCheckbox
              v-for="m in g.models"
              :key="m"
              :checked="isSelected(m)"
              :disabled="isAdded(m)"
              @update:checked="(v: boolean) => toggle(m, v)"
            >
              <span class="preset-model">
                <span class="preset-model__id">{{ m }}</span>
                <n-tag v-if="isAdded(m)" size="tiny" :bordered="false">{{ t('models.presetAdded') }}</n-tag>
              </span>
            </NCheckbox>
          </div>
        </div>
      </div>
    </div>

    <n-form require-mark-placement="left" ref="formRef" :model="form" :rules="rules" class="manual-form">
      <n-form-item path="name">
        <template #label>
          <HelpLabel :tip="t('models.name_tip')">{{ t('models.nameManual') }}</HelpLabel>
        </template>
        <n-input v-model:value="form.name" :placeholder="t('models.nameHint')" />
      </n-form-item>
      <n-form-item path="schedulingMode">
        <template #label>
          <HelpLabel :tip="t('models.schedulingModeCreate_tip')">{{ t('models.schedulingMode') }}</HelpLabel>
        </template>
        <n-select v-model:value="form.schedulingMode" :options="schedulingModeOptions" />
      </n-form-item>
    </n-form>

    <template #footer>
      <n-space justify="end" align="center">
        <span v-if="selected.size > 0" class="selected-count">
          {{ t('models.presetSelectedCount', { count: selected.size }) }}
        </span>
        <n-button @click="showModel = false">{{ t('models.cancel') }}</n-button>
        <n-button type="primary" :loading="submitting" @click="onSubmit">{{ t('models.save') }}</n-button>
      </n-space>
    </template>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { NCheckbox, NSelect, useMessage, type FormInst, type FormItemRule, type FormRules } from 'naive-ui'
import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { modelNameFormatRule } from '../../utils/modelValidators'
import { useSchedulingModeOptions } from '../../utils/schedulingMode'
import { MODEL_PRESET_GROUPS, type ModelPresetGroup } from '../../config/modelPresets'
import type { SchedulingMode } from '../../api/models'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ 'update:show': [boolean] }>()

// ModalDrawer owns a v-model:show; bridge it to this component's existing
// :show / @update:show contract so the parent doesn't have to change.
const showModel = computed({
  get: () => props.show,
  set: (v) => emit('update:show', v),
})

const { t } = useI18n()
const message = useMessage()
const router = useRouter()
const store = useModelsStore()

const formRef = ref<FormInst | null>(null)
const submitting = ref(false)
// form.schedulingMode applies to everything this submission creates — the
// manual name and any preset picks alike.
const form = reactive<{ name: string; schedulingMode: SchedulingMode }>({ name: '', schedulingMode: 'balanced' })
const selected = ref(new Set<string>())

const schedulingModeOptions = useSchedulingModeOptions()

// Names already present, so preset entries for them can be disabled + badged.
const existingNames = computed(() => new Set(store.list.map((m) => m.name)))
function isAdded(name: string): boolean {
  return existingNames.value.has(name)
}

// The manual name is optional whenever presets are selected, otherwise
// required — so the required half is a validator, composed with the shared
// charset/length rule (which skips empty input on its own).
const rules: FormRules = {
  name: [
    {
      trigger: ['blur', 'input'],
      validator: (_rule: FormItemRule, value: string) => {
        if (!(value ?? '').trim() && selected.value.size === 0) return new Error(t('models.fieldRequired'))
        return true
      },
    },
    modelNameFormatRule(t),
  ],
}

// Each group with its localized vendor heading.
const groups = computed(() => MODEL_PRESET_GROUPS.map((g) => ({ ...g, name: t(`models.presetVendor_${g.vendorId}`) })))

function isSelected(name: string): boolean {
  return selected.value.has(name)
}
function toggle(name: string, checked: boolean) {
  if (checked) selected.value.add(name)
  else selected.value.delete(name)
}

// Group-level select-all only ever touches the not-yet-added models.
function groupSelectable(g: ModelPresetGroup): string[] {
  return g.models.filter((m) => !isAdded(m))
}
function groupChecked(g: ModelPresetGroup): boolean {
  const sel = groupSelectable(g)
  return sel.length > 0 && sel.every(isSelected)
}
function groupIndeterminate(g: ModelPresetGroup): boolean {
  const sel = groupSelectable(g)
  const picked = sel.filter(isSelected).length
  return picked > 0 && picked < sel.length
}
function toggleGroup(g: ModelPresetGroup, checked: boolean) {
  for (const m of groupSelectable(g)) {
    if (checked) selected.value.add(m)
    else selected.value.delete(m)
  }
}

watch(
  () => props.show,
  (visible) => {
    if (visible) {
      form.name = ''
      form.schedulingMode = 'balanced'
      selected.value = new Set()
    }
  },
)

async function onSubmit() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  const manual = form.name.trim()

  // No presets picked → single manual create, keeping the original
  // "create then jump into the new model's detail page" behavior.
  if (selected.value.size === 0) {
    submitting.value = true
    try {
      const created = await store.create(manual, form.schedulingMode)
      showModel.value = false
      router.push(`/models/${created.id}`)
    } catch (err) {
      message.error(displayMessage(err, t))
    } finally {
      submitting.value = false
    }
    return
  }

  // Presets picked → batch create (folding in the manual name if given).
  const names = new Set(selected.value)
  if (manual) names.add(manual)
  submitting.value = true
  try {
    const result = await store.createBatch([...names], form.schedulingMode)
    const created = result.created.length
    const skipped = result.skipped.length
    if (created === 0) {
      message.warning(t('models.batchNothingAdded'))
    } else if (skipped === 0) {
      message.success(t('models.batchSummarySuccess', { created }))
    } else {
      message.warning(t('models.batchSummaryWithSkip', { created, skipped }))
    }
    showModel.value = false
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
.preset-section {
  margin-bottom: var(--space-4);
}
.preset-label {
  display: block;
  margin-bottom: var(--space-3);
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
}
.preset-groups {
  max-height: 320px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  padding-right: var(--space-2);
}
.preset-group__name {
  font-weight: 600;
}
.preset-group__models {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2) var(--space-5);
  margin-top: var(--space-2);
  padding-left: var(--space-6);
}
.preset-model {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
}
.preset-model__id {
  font-family: var(--font-mono);
  font-size: 13px;
}
.manual-form {
  padding-top: var(--space-4);
  border-top: 1px solid var(--color-border-subtle);
}
.selected-count {
  margin-right: auto;
  color: var(--color-text-muted);
  font-size: 13px;
}
</style>
