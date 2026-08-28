<!-- frontend/src/components/providers/DeleteProviderModal.vue
     Danger confirmation for deleting a provider. States what the cascade
     removes, previews the routing impact, promises that history stays, and
     gates the destructive button behind retyping the provider's exact name. -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="t('providers.deleteProvider')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :back-label="t('common.back')"
    @after-leave="reset"
  >
    <div class="delete-provider">
      <p class="delete-provider__line delete-provider__intro">
        {{ t('providers.deleteProviderIntro', { name: providerName }) }}
      </p>
      <div v-if="impactLoading" class="delete-provider__loading"><n-spin size="small" /></div>
      <template v-else>
        <p v-for="line in impactDisplayLines" :key="line" class="delete-provider__line">{{ line }}</p>
        <n-alert v-if="strandedCount > 0" type="error" :show-icon="false" class="delete-provider__note">
          {{ t('providers.deleteProviderSevereWarning', { count: strandedCount }) }}
        </n-alert>
      </template>
      <n-alert type="warning" :show-icon="false" class="delete-provider__note">
        {{ t('providers.deleteProviderHistoryNote') }}
      </n-alert>
      <p class="delete-provider__confirm-label">
        {{ t('providers.deleteProviderTypeToConfirm', { name: providerName }) }}
      </p>
      <n-input
        v-model:value="confirmInput"
        :placeholder="providerName"
        :input-props="{ 'aria-label': t('providers.deleteProviderTypeToConfirm', { name: providerName }) }"
      />
    </div>
    <template #footer>
      <n-space justify="end">
        <n-button @click="showModel = false">{{ t('providers.cancel') }}</n-button>
        <n-button type="error" :loading="deleting" :disabled="!unlocked" @click="onDelete">
          {{ t('common.delete') }}
        </n-button>
      </n-space>
    </template>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { NAlert, NButton, NInput, NSpace, NSpin, useMessage } from 'naive-ui'
import { useI18n } from 'vue-i18n'
import ModalDrawer from '../common/ModalDrawer.vue'
import { useProvidersStore } from '../../store/providers'
import { getProviderImpact, type Provider, type ProviderImpact } from '../../api/providers'
import { providerDeleteImpactView } from '../../utils/impactSummary'
import { deleteConfirmUnlocked } from '../../utils/providerStatusDisplay'
import { displayMessage } from '../../api/client'

const props = defineProps<{ show: boolean; provider: Provider | null }>()
const emit = defineEmits<{ (e: 'update:show', v: boolean): void; (e: 'deleted'): void }>()

const { t } = useI18n()
const message = useMessage()
const store = useProvidersStore()

const showModel = computed({
  get: () => props.show,
  set: (v) => emit('update:show', v),
})
// Snapshot, not a pass-through: on the list page the row is nulled the
// moment the modal starts closing, and the name must not blank out of the
// copy mid-fade.
const providerName = ref('')
watch(
  () => props.provider,
  (p) => {
    if (p !== null) providerName.value = p.name
  },
  { immediate: true },
)

const confirmInput = ref('')
const deleting = ref(false)
const impactLoading = ref(false)
// The raw impact answer is stored and projected in a computed, so the lines
// re-render in the current language if the admin switches locale while the
// modal sits open for the retype gate.
const impactRaw = ref<ProviderImpact | null>(null)
const impactFailed = ref(false)

const impactView = computed(() => (impactRaw.value === null ? null : providerDeleteImpactView(impactRaw.value, t)))
const impactDisplayLines = computed(() =>
  // A broken preview must never block the action — say the summary is
  // unavailable instead of showing nothing.
  impactFailed.value || impactView.value === null
    ? [t('providers.deleteProviderImpactUnavailable')]
    : impactView.value.lines,
)
const strandedCount = computed(() => impactView.value?.strandedCount ?? 0)
const unlocked = computed(() => deleteConfirmUnlocked(confirmInput.value, providerName.value))

watch(
  () => props.show,
  (open) => {
    // The retype gate dies with the close signal itself, not only on
    // after-leave: a mobile→desktop breakpoint cross destroys the drawer
    // branch without a leave transition, and a retype that survived it
    // would arm the delete button on the next open with no retyping. The
    // impact lines stay for the fade; after-leave's reset clears them, and
    // the open-watch refetch covers the no-leave path.
    if (!open) {
      confirmInput.value = ''
      return
    }
    const requestedId = props.provider?.id
    if (requestedId === undefined) return
    impactLoading.value = true
    impactFailed.value = false
    // On the list page one modal instance serves every row; a slow answer
    // for a previously opened provider must not render under the name of
    // the one currently on screen.
    const stale = () => !props.show || props.provider?.id !== requestedId
    getProviderImpact(requestedId)
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
  confirmInput.value = ''
  impactRaw.value = null
  impactFailed.value = false
  impactLoading.value = false
}

async function onDelete() {
  if (props.provider === null || !unlocked.value) return
  deleting.value = true
  try {
    await store.deleteProvider(props.provider.id)
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
.delete-provider__line {
  margin: 0 0 8px;
  font-size: 13px;
  line-height: 1.6;
}
.delete-provider__intro {
  font-weight: 500;
}
.delete-provider__loading {
  margin: 0 0 8px;
}
.delete-provider__note {
  margin: 4px 0 12px;
}
.delete-provider__confirm-label {
  margin: 0 0 6px;
  font-size: 13px;
  color: var(--color-text-secondary);
}
</style>
