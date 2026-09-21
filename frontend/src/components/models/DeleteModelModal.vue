<!-- frontend/src/components/models/DeleteModelModal.vue
     Danger confirmation for deleting a model. States what the cascade
     removes, previews the impact (candidate count, caller keys, recent
     traffic), and promises that history stays. A plain two-button confirm —
     no retype gate, unlike the provider delete: no secret is exposed, all
     history is retained, and the model can be recreated under the same
     name at any time. -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="t('models.deleteModel')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :back-label="t('common.back')"
    @after-leave="reset"
  >
    <div class="delete-model">
      <p class="delete-model__line delete-model__intro">
        {{ t('models.deleteModelIntro', { name: modelName }) }}
      </p>
      <div v-if="impactLoading" class="delete-model__loading"><n-spin size="small" /></div>
      <template v-else>
        <p v-for="line in impactDisplayLines" :key="line" class="delete-model__line">{{ line }}</p>
      </template>
      <n-alert type="warning" :show-icon="false" class="delete-model__note">
        {{ historyNote }}
      </n-alert>
    </div>
    <template #footer>
      <n-space justify="end">
        <n-button @click="showModel = false">{{ t('models.cancel') }}</n-button>
        <n-button type="error" :loading="deleting" @click="onDelete">
          {{ t('common.delete') }}
        </n-button>
      </n-space>
    </template>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { NAlert, NButton, NSpace, NSpin, useMessage } from 'naive-ui'
import { useI18n } from 'vue-i18n'
import ModalDrawer from '../common/ModalDrawer.vue'
import { useModelsStore } from '../../store/models'
import { getModelImpact, type Model, type ModelImpact } from '../../api/models'
import { modelDeleteImpactView } from '../../utils/impactSummary'
import { displayMessage } from '../../api/client'

const props = defineProps<{ show: boolean; model: Model | null }>()
const emit = defineEmits<{ (e: 'update:show', v: boolean): void; (e: 'deleted'): void }>()

const { t } = useI18n()
const message = useMessage()
const store = useModelsStore()

const showModel = computed({
  get: () => props.show,
  set: (v: boolean) => emit('update:show', v),
})
// Snapshot, not a pass-through: on the list page the row is nulled the
// moment the modal starts closing, and the name must not blank out of the
// copy mid-fade.
const modelName = ref('')
watch(
  () => props.model,
  (m) => {
    if (m !== null) modelName.value = m.name
  },
  { immediate: true },
)

const deleting = ref(false)
const impactLoading = ref(false)
// The raw impact answer is stored and projected in a computed, so the lines
// re-render in the current language if the admin switches locale while the
// modal sits open.
const impactRaw = ref<ModelImpact | null>(null)
const impactFailed = ref(false)

const impactView = computed(() => (impactRaw.value === null ? null : modelDeleteImpactView(impactRaw.value, t)))
const impactDisplayLines = computed(() =>
  // A broken preview must never block the action — say the summary is
  // unavailable instead of showing nothing (same degrade contract as the
  // disable dialogs).
  impactFailed.value || impactView.value === null
    ? [t('models.deleteModelImpactUnavailable')]
    : impactView.value.lines,
)
// The retention promise is fixed copy, not impact data, so it shows even
// when the preview failed.
const historyNote = computed(() => t('models.deleteModelHistoryNote'))

watch(
  () => props.show,
  (open) => {
    if (!open) return
    const requestedId = props.model?.id
    if (requestedId === undefined) return
    impactLoading.value = true
    impactFailed.value = false
    // On the list page one modal instance serves every row; a slow answer
    // for a previously opened model must not render under the name of the
    // one currently on screen.
    const stale = () => !props.show || props.model?.id !== requestedId
    getModelImpact(requestedId)
      .then((impact) => {
        if (stale()) return
        impactRaw.value = impact
        impactLoading.value = false
      })
      .catch(() => {
        if (stale()) return
        impactFailed.value = true
        impactLoading.value = false
      })
  },
)

function reset() {
  impactRaw.value = null
  impactFailed.value = false
  impactLoading.value = false
}

async function onDelete() {
  if (props.model === null) return
  deleting.value = true
  try {
    await store.deleteModel(props.model.id)
    showModel.value = false
    emit('deleted')
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    deleting.value = false
  }
}
</script>

<style scoped>
.delete-model__line {
  margin: 0 0 8px;
  font-size: 13px;
  line-height: 1.6;
}
.delete-model__intro {
  font-weight: 500;
}
.delete-model__loading {
  margin: 0 0 8px;
}
.delete-model__note {
  margin: 4px 0 12px;
}
</style>
