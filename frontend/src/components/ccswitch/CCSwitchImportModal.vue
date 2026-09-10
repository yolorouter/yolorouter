<!-- frontend/src/components/ccswitch/CCSwitchImportModal.vue
     The shared CC-Switch export dialog. Key-picking mode (models page) is a
     later ticket; this file currently implements the model-picking mode the
     API-keys page opens:

       - the key half is FIXED (shown as owner (#id) + prefix),
       - the model half is chosen: gateway discovery authed with this key is
         the primary source, the admin catalog annotates availability and
         backfills on discovery failure (planning rules live in the pure
         utils/ccswitchExport module), and manual entry is the last resort
         (empty allowed — the deep link's model param is optional).

     The dialog opens before any data arrives (loading state), and its
     Confirm fires with zero awaits — everything is prefetched during
     selection, so the external-protocol navigation leaves while the click's
     transient user activation is still live.

     On confirm it emits the pair and closes; the OPENING page owns the
     profile name and the deep-link launch (useCCSwitchImport). -->
<template>
  <ModalDrawer
    v-model:show="show"
    :title="t('ccswitch.modalTitle')"
    :confirm-text="t('ccswitch.confirmLaunchButton')"
    :back-label="t('common.back')"
    :loading="phase === 'loading'"
    :confirm-disabled="!canConfirm"
    @confirm="onConfirm"
  >
    <div class="ccs-import">
      <div class="ccs-import__field">
        <span class="ccs-import__label">{{ t('ccswitch.keyLabel') }}</span>
        <div class="ccs-import__key mono">
          <span>{{ keyIdentity }}</span>
          <span v-if="apiKeyRow" class="ccs-import__prefix">{{ apiKeyRow.key_prefix }}…</span>
        </div>
      </div>

      <div class="ccs-import__field">
        <span class="ccs-import__label">{{ t('ccswitch.modelLabel') }}</span>
        <div v-if="phase === 'loading'" class="ccs-import__hint">
          {{ t('ccswitch.modelLoading') }}
        </div>
        <div v-else-if="phase === 'error'" class="ccs-import__error">
          <span class="ccs-import__hint">{{ loadError }}</span>
          <NButton size="small" @click="load">{{ t('ccswitch.retry') }}</NButton>
        </div>
        <!-- Not clearable: a confirmed export always carries a model choice;
             exporting without one is the manual-fallback path below. -->
        <NSelect
          v-else-if="plan?.mode === 'select'"
          v-model:value="selected"
          :options="selectOptions"
          filterable
        />
        <template v-else>
          <div class="ccs-import__hint">{{ t('ccswitch.modelManualHint') }}</div>
          <NInput
            v-model:value="manualModel"
            :placeholder="t('ccswitch.modelPlaceholder')"
          />
        </template>
      </div>

      <div v-if="legacy" class="ccs-import__notice">
        {{ t('ccswitch.plaintextUnavailable') }}
      </div>

      <!-- Degraded, not broken: the fallback list below works, but it is
           second-best, and one retry might restore the discovered one. -->
      <div v-if="phase === 'ready' && discoveryFailed" class="ccs-import__notice ccs-import__notice--row">
        <span>{{ t('ccswitch.discoveryFailedHint') }}</span>
        <NButton size="tiny" quaternary @click="load">{{ t('ccswitch.retry') }}</NButton>
      </div>
    </div>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, h, ref, watch, type VNodeChild } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NInput, NSelect, NTag } from 'naive-ui'

import ModalDrawer from '../common/ModalDrawer.vue'
import {
  discoverGatewayModels,
  getAPIKeyPlaintext,
  ERRCODE_KEY_PLAINTEXT_UNAVAILABLE,
  type APIKey,
} from '../../api/apiKeys'
import { displayMessage, errorCodeOf } from '../../api/client'
import {
  planCCSwitchModelChoices,
  type CCSwitchCatalogModel,
  type CCSwitchConfirmPayload,
  type CCSwitchModelPlan,
} from '../../utils/ccswitchExport'

const props = defineProps<{
  // The fixed half: the key being exported, or null while the dialog is
  // closed (useRowModal pattern — the opener clears the row on close).
  // Owner-scoped on the server, so members reach their own keys and admins
  // any key they can see.
  apiKeyRow: APIKey | null
  // The viewer's admin catalog, or null when the viewer cannot read one
  // (member sessions) — availability is then unknown and no backfill exists.
  catalog: CCSwitchCatalogModel[] | null
}>()

const emit = defineEmits<{
  confirm: [CCSwitchConfirmPayload]
}>()

const { t } = useI18n()

const show = defineModel<boolean>('show', { required: true })

type Phase = 'loading' | 'ready' | 'error'
const phase = ref<Phase>('loading')
const loadError = ref('')
const plan = ref<CCSwitchModelPlan | null>(null)
const selected = ref<string | null>(null)
const manualModel = ref('')
// Undefined only for a legacy key (11016): the export then carries the
// composable's placeholder and the notice below tells the user to paste.
const plaintext = ref<string | undefined>(undefined)
const legacy = ref(false)
// Discovery ran and failed (transient): the picker still works off the
// fallback sources, but the user deserves to know the list is second-best
// and to have the retry that might restore the real one.
const discoveryFailed = ref(false)

// Same identity rule the exported profile name uses, so the row the dialog
// shows and the name CC-Switch receives can never disagree.
const keyIdentity = computed(() => {
  const row = props.apiKeyRow
  if (!row) return ''
  return row.owner_username ? `${row.owner_username} (#${row.id})` : `#${row.id}`
})

const canConfirm = computed(
  () => phase.value === 'ready' && (plan.value?.mode === 'manual' || !!selected.value),
)

const selectOptions = computed(() => {
  if (plan.value?.mode !== 'select') return []
  return plan.value.choices.map((c) => ({
    label: c.name,
    value: c.name,
    render: (): VNodeChild =>
      c.available === null
        ? c.name
        : h('span', { class: 'ccs-import__option' }, [
            h('span', null, c.name),
            h(
              NTag,
              { size: 'tiny', bordered: false, type: c.available ? 'success' : 'warning' },
              {
                default: () =>
                  c.available ? t('ccswitch.statusAvailable') : t('ccswitch.statusUnavailable'),
              },
            ),
          ]),
  }))
})

// Monotonic token: reopening for another row (or a retry) while a previous
// load is still in flight must leave the LAST started load authoritative —
// same guard pattern the list stores use for stale responses.
let loadId = 0

async function load() {
  const id = ++loadId
  phase.value = 'loading'
  plan.value = null
  selected.value = null
  manualModel.value = ''
  legacy.value = false
  discoveryFailed.value = false
  plaintext.value = undefined
  loadError.value = ''
  if (!props.apiKeyRow) return

  try {
    plaintext.value = (await getAPIKeyPlaintext(props.apiKeyRow.id)).plaintext_key
  } catch (err) {
    if (errorCodeOf(err) !== ERRCODE_KEY_PLAINTEXT_UNAVAILABLE) {
      // Transient — importing the placeholder would silently hand
      // CC-Switch a wrong key for a perfectly readable credential.
      if (id !== loadId) return
      phase.value = 'error'
      loadError.value = displayMessage(err, t)
      return
    }
    // Legacy key: placeholder export continues, but discovery has nothing
    // to authenticate with, so the planner runs on the fallback sources.
    legacy.value = true
  }

  let discovered: string[] | null = null
  if (plaintext.value) {
    // A flaked gateway must not block the export — the planner degrades to
    // catalog backfill or manual entry — but the degradation is surfaced
    // (discoveryFailed) with a retry, not passed off silently.
    try {
      discovered = await discoverGatewayModels(plaintext.value)
    } catch {
      discovered = null
      discoveryFailed.value = true
    }
  }
  if (id !== loadId) return

  plan.value = planCCSwitchModelChoices({
    discovered,
    catalog: props.catalog,
    key: {
      allow_all_models: props.apiKeyRow.allow_all_models,
      model_ids: props.apiKeyRow.model_ids,
    },
  })
  selected.value = plan.value.mode === 'select' ? plan.value.preselect : null
  phase.value = 'ready'
}

// Reload on every open and every row change: the dialog is reused across
// rows, and a stale plan from the previous row would preselect a model this
// key cannot route to.
watch(
  () => [show.value, props.apiKeyRow] as const,
  ([isOpen]) => {
    if (isOpen) void load()
  },
  { immediate: true },
)

function onConfirm() {
  if (!canConfirm.value) return
  emit('confirm', {
    apiKey: plaintext.value,
    model:
      plan.value?.mode === 'select'
        ? (selected.value ?? undefined)
        : manualModel.value.trim() || undefined,
  })
  show.value = false
}
</script>

<style scoped>
.ccs-import {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.ccs-import__field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.ccs-import__label {
  font-size: var(--text-xs);
  color: var(--color-text-secondary);
}

.ccs-import__key {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
  min-width: 0;
}

.ccs-import__prefix {
  color: var(--color-text-secondary);
}

.mono {
  font-family: var(--font-mono, monospace);
  font-weight: 600;
}

.ccs-import__hint {
  font-size: var(--text-xs);
  color: var(--color-text-secondary);
}

.ccs-import__error {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-2);
}

.ccs-import__notice {
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  font-size: var(--text-xs);
  color: var(--color-text-secondary);
}

.ccs-import__notice--row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
}

:deep(.ccs-import__option) {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
</style>
