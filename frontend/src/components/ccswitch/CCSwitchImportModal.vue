<!-- frontend/src/components/ccswitch/CCSwitchImportModal.vue
     The shared CC-Switch export dialog, in two modes:

       - 'model' (API-keys page): the key half is FIXED (shown as owner
         (#id) + prefix), the model half is chosen — gateway discovery
         authed with this key is the primary source, the admin catalog
         annotates availability and backfills on discovery failure
         (planning rules live in the pure utils/ccswitchExport module),
         and manual entry is the last resort (empty allowed — the deep
         link's model param is optional).
       - 'key' (models page): the model half is FIXED, the key half is
         chosen — the viewer's own active keys whose routing scope covers
         the model (the pure filter in utils/ccswitchExport), fetched page
         by page; the selected key's plaintext is prefetched the moment
         the selection lands.

     Both modes open before any data arrives (loading state), surface
     failures in-modal with retry, never fall back to the placeholder for a
     transient failure (only the permanent 11016 legacy case does), and
     fire Confirm with zero awaits — everything is prefetched during
     selection, so the external-protocol navigation leaves while the
     click's transient user activation is still live.

     On confirm it emits the pair and closes; the OPENING page owns the
     profile name and the deep-link launch (useCCSwitchImport). -->
<template>
  <ModalDrawer
    v-model:show="show"
    :title="t('ccswitch.modalTitle')"
    :confirm-text="t('common.confirm')"
    :back-label="t('common.back')"
    :loading="phase === 'loading'"
    :confirm-disabled="!canConfirm"
    @confirm="onConfirm"
  >
    <div class="ccs-import">
      <div class="ccs-import__field">
        <span class="ccs-import__label">{{ t(fixedHalfLabelKey) }}</span>
        <div v-if="mode === 'key'" class="ccs-import__key mono">{{ fixedModelName }}</div>
        <div v-else class="ccs-import__key mono">
          <span>{{ keyIdentity }}</span>
          <span v-if="apiKeyRow" class="ccs-import__prefix">{{ apiKeyRow.key_prefix }}…</span>
        </div>
      </div>

      <div class="ccs-import__field">
        <span class="ccs-import__label">{{ t(pickHalfLabelKey) }}</span>
        <div v-if="phase === 'loading'" class="ccs-import__hint">
          {{ t(pickHalfLoadingKey) }}
        </div>
        <div v-else-if="phase === 'error'" class="ccs-import__error">
          <span class="ccs-import__hint">{{ loadError }}</span>
          <NButton size="small" @click="runLoad">{{ t('ccswitch.retry') }}</NButton>
        </div>
        <template v-else-if="mode === 'key'">
          <EmptyState
            v-if="keyChoices.length === 0"
            :icon="KeyRound"
            :title="t('ccswitch.keysEmptyTitle')"
            :description="t('ccswitch.keysEmptyHint')"
          >
            <template #action>
              <NButton type="primary" size="small" @click="goCreateKey">
                {{ t('apiKeys.createButton') }}
              </NButton>
            </template>
          </EmptyState>
          <template v-else>
            <!-- Switching the selection re-runs the plaintext prefetch and
                 withholds Confirm until it settles — a confirm must never
                 carry one key's credential under another's selection. -->
            <NSelect v-model:value="selectedKeyId" :options="keySelectOptions" filterable />
            <div v-if="keyPrefetchError" class="ccs-import__notice ccs-import__notice--row">
              <span>{{ keyPrefetchError }}</span>
              <NButton
                size="tiny"
                quaternary
                @click="prefetchSelectedKey(selectedKeyId)"
              >
                {{ t('ccswitch.retry') }}
              </NButton>
            </div>
          </template>
        </template>
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
      <div
        v-if="mode !== 'key' && phase === 'ready' && discoveryFailed"
        class="ccs-import__notice ccs-import__notice--row"
      >
        <span>{{ t('ccswitch.discoveryFailedHint') }}</span>
        <NButton size="tiny" quaternary @click="runLoad">{{ t('ccswitch.retry') }}</NButton>
      </div>
    </div>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, h, ref, watch, type VNodeChild } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { NButton, NInput, NSelect, NTag } from 'naive-ui'
import { KeyRound } from '@lucide/vue'

import ModalDrawer from '../common/ModalDrawer.vue'
import EmptyState from '../EmptyState.vue'
import {
  discoverGatewayModels,
  getAPIKeyPlaintext,
  listAPIKeys,
  ERRCODE_KEY_PLAINTEXT_UNAVAILABLE,
  type APIKey,
} from '../../api/apiKeys'
import { displayMessage, errorCodeOf } from '../../api/client'
import {
  ccsKeyIdentity,
  filterCCSwitchCompatibleKeys,
  planCCSwitchModelChoices,
  type CCSwitchCatalogModel,
  type CCSwitchConfirmPayload,
  type CCSwitchKeyRow,
  type CCSwitchModelPlan,
} from '../../utils/ccswitchExport'

const props = withDefaults(
  defineProps<{
    // 'model' (API-keys page): the key half is fixed, the model half is
    // chosen. 'key' (models page): the model half is fixed, the key half is
    // chosen.
    mode?: 'model' | 'key'
    // model mode: the fixed key being exported, or null while closed
    // (useRowModal pattern — the opener clears the row on close).
    // Owner-scoped on the server, so members reach their own keys and
    // admins any key they can see.
    apiKeyRow?: APIKey | null
    // model mode: the viewer's admin catalog, or null when the viewer
    // cannot read one (member sessions) — availability is then unknown and
    // no backfill exists.
    catalog?: CCSwitchCatalogModel[] | null
    // key mode: the fixed model being exported (or null while closed).
    modelRow?: { id: number; name: string } | null
    // key mode: the login username — the picker lists only this account's
    // own keys (/auth/me carries no numeric id; the username is the unique
    // login identity).
    ownerUsername?: string
  }>(),
  {
    mode: 'model',
    apiKeyRow: null,
    catalog: null,
    modelRow: null,
    ownerUsername: '',
  },
)

const emit = defineEmits<{
  confirm: [CCSwitchConfirmPayload]
}>()

const { t } = useI18n()
const router = useRouter()

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

// --- key mode state --------------------------------------------------------
const keyChoices = ref<CCSwitchKeyRow[]>([])
const selectedKeyId = ref<number | null>(null)
// True once the SELECTED key's plaintext prefetch settled (plaintext or the
// legacy placeholder) — Confirm stays withheld across a selection switch
// until the new key's prefetch settles too.
const plaintextReady = ref(false)
const keyPrefetchError = ref('')

// Same identity the exported profile name is built from — see
// ccsKeyIdentity, the one home of the rule.
const keyIdentity = computed(() => (props.apiKeyRow ? ccsKeyIdentity(props.apiKeyRow) : ''))

// Key mode: the fixed model half.
const fixedModelName = computed(() => props.modelRow?.name ?? '')

// The fixed half and the pickable half swap labels between modes — one
// home each so the template never mirrors the mapping.
const fixedHalfLabelKey = computed(() =>
  props.mode === 'key' ? 'ccswitch.modelLabel' : 'ccswitch.keyLabel',
)
const pickHalfLabelKey = computed(() =>
  props.mode === 'key' ? 'ccswitch.keyLabel' : 'ccswitch.modelLabel',
)
const pickHalfLoadingKey = computed(() =>
  props.mode === 'key' ? 'ccswitch.keysLoading' : 'ccswitch.modelLoading',
)

const canConfirm = computed(() => {
  if (phase.value !== 'ready') return false
  if (props.mode === 'key') {
    return keyChoices.value.length > 0 && selectedKeyId.value !== null && plaintextReady.value
  }
  return plan.value?.mode === 'manual' || !!selected.value
})

const keySelectOptions = computed(() =>
  keyChoices.value.map((k) => ({
    // remark is the owner-given label; the prefix tells keys apart when
    // it's empty.
    label: k.remark || k.key_prefix,
    value: k.id,
  })),
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
  keyChoices.value = []
  // Setting null fires the selection watcher, which resets the prefetch
  // state — one reset path for open, close, and row change alike.
  selectedKeyId.value = null
  plaintextReady.value = false
  keyPrefetchError.value = ''

  if (props.mode === 'key') {
    await loadKeyChoices(id)
    return
  }

  // Capture the row: closing the modal clears it (useRowModal), and while
  // the close also bumps loadId, this load must never read props.apiKeyRow
  // past its first await — the capture is the row this load belongs to,
  // independent of how the guards interleave.
  const row = props.apiKeyRow
  if (!row) return

  try {
    const { plaintext_key } = await getAPIKeyPlaintext(row.id)
    // Guard the SUCCESS write too, not just the catch path: a stale load
    // resolving after a newer one must never overwrite the newer row's
    // plaintext — the confirm would then export this profile name with the
    // previous row's credential.
    if (id !== loadId) return
    plaintext.value = plaintext_key
  } catch (err) {
    // Guard BEFORE any catch-path write: a stale load whose request
    // rejects after a newer load reset the flags must stay silent —
    // otherwise it pins its legacy/notice state onto the newer row.
    if (id !== loadId) return
    if (errorCodeOf(err) !== ERRCODE_KEY_PLAINTEXT_UNAVAILABLE) {
      // Transient — importing the placeholder would silently hand
      // CC-Switch a wrong key for a perfectly readable credential.
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
      if (id === loadId) discoveryFailed.value = true
    }
  }
  if (id !== loadId) return

  plan.value = planCCSwitchModelChoices({
    discovered,
    catalog: props.catalog,
    key: {
      allow_all_models: row.allow_all_models,
      model_ids: row.model_ids,
    },
  })
  selected.value = plan.value.mode === 'select' ? plan.value.preselect : null
  phase.value = 'ready'
}

// Key mode's list half: the viewer's own active keys whose scope covers
// the fixed model, preselecting the first. The scope/owner/status rule
// lives in the pure filter; this owns fetching everything the filter sees —
// the shared keys store's pagination state belongs to the keys page and
// must not be driven from here.
async function loadKeyChoices(id: number) {
  const row = props.modelRow
  if (!row) return
  try {
    const rows: APIKey[] = []
    const seen = new Set<number>()
    let page = 1
    // page_size caps at 200 server-side; loop until the reported total is
    // collected. The empty-page break keeps a miscounted total from
    // spinning forever, and the seen-set drops the duplicate a key
    // created-or-revoked between page fetches can otherwise re-list.
    for (;;) {
      const res = await listAPIKeys({ q: '', status: 'active', page, pageSize: 200 })
      if (id !== loadId) return
      for (const k of res.list) {
        if (!seen.has(k.id)) {
          seen.add(k.id)
          rows.push(k)
        }
      }
      if (rows.length >= res.total || res.list.length === 0) break
      page++
    }
    keyChoices.value = filterCCSwitchCompatibleKeys(rows, row.id, props.ownerUsername)
    // The assignment fires the selection watcher, which prefetches this
    // key's plaintext and re-arms the confirm gate.
    selectedKeyId.value = keyChoices.value[0]?.id ?? null
    phase.value = 'ready'
  } catch (err) {
    if (id !== loadId) return
    phase.value = 'error'
    loadError.value = displayMessage(err, t)
  }
}

// Key mode's credential half: the SELECTED key's plaintext, prefetched the
// moment the selection lands so Confirm never awaits. Switching the
// selection bumps the token — a slow prefetch for a deselected key must
// not arm the gate or leak its plaintext into the confirm payload.
let selToken = 0
async function prefetchSelectedKey(id: number | null) {
  const token = ++selToken
  plaintext.value = undefined
  legacy.value = false
  plaintextReady.value = false
  keyPrefetchError.value = ''
  if (id == null) return
  try {
    const { plaintext_key } = await getAPIKeyPlaintext(id)
    if (token !== selToken) return
    plaintext.value = plaintext_key
  } catch (err) {
    if (token !== selToken) return
    if (errorCodeOf(err) === ERRCODE_KEY_PLAINTEXT_UNAVAILABLE) {
      // Legacy key: placeholder export continues, notice shown.
      legacy.value = true
    } else {
      // Transient — never fall back to the placeholder for a readable
      // credential; Confirm stays withheld until a retry succeeds.
      keyPrefetchError.value = displayMessage(err, t)
      return
    }
  }
  plaintextReady.value = true
}

// Reload on every open and every row change: the dialog is reused across
// rows, and a stale plan from the previous row would preselect a model this
// key cannot route to. Closing suppresses instead: a load dismissed by the
// close has its writes discarded (the HTTP request itself is not
// transport-aborted, but its result is dropped, and discovery never fires
// with the dismissed credential).
watch(
  () => [show.value, props.apiKeyRow, props.modelRow] as const,
  ([isOpen]) => {
    if (isOpen) void runLoad()
    else loadId++
  },
  { immediate: true },
)

// Key mode: the selection drives the credential prefetch. Fires on the
// load()'s resets (null) and on preselect/switch alike.
watch(selectedKeyId, (id) => {
  if (props.mode === 'key') void prefetchSelectedKey(id)
})

// Defensive outer net: every realistic failure is caught inside load; a
// defect in there must not strand the dialog in the loading phase with the
// confirm button silently withheld.
async function runLoad() {
  try {
    await load()
  } catch (err) {
    phase.value = 'error'
    loadError.value = displayMessage(err, t)
  }
}

// The empty picker's way out: hand the user over to where keys are made.
function goCreateKey() {
  show.value = false
  void router.push('/api-keys')
}

function onConfirm() {
  if (!canConfirm.value) return
  let model: string | undefined
  if (props.mode === 'key') {
    model = props.modelRow?.name
  } else if (plan.value?.mode === 'select') {
    model = selected.value ?? undefined
  } else {
    model = manualModel.value.trim() || undefined
  }
  emit('confirm', {
    apiKey: plaintext.value,
    model,
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
