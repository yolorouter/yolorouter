<!-- frontend/src/views/providers/ProviderDetailPage.vue -->
<template>
  <div class="common-page" v-if="provider">
    <PageHeader class="actions-placeholder" :eyebrow="t('providers.eyebrow')" :title="provider.name" :description="provider.base_url">
      <!-- The proxy warning rides the title: the header is where the page
           states what this provider IS, and "its traffic leaves through a
           proxy" belongs in that sentence. -->
      <template #title-suffix>
        <ProviderProxyWarning v-if="providerProxied" />
      </template>
      <template #actions>
        <template v-if="!isMobile">
          <n-button size="small" @click="showEditProvider = true">{{ t('providers.editProvider') }}</n-button>
          <n-button size="small" @click="router.push(`/costs/providers/${provider.id}`)">
            {{ t('costs.detail.viewCost') }}
          </n-button>
          <n-button size="small" @click="onToggleProviderStatus">
            {{ provider.management_status === 1 ? t('providers.statusDisabled') : t('providers.statusEnabled') }}
          </n-button>
          <n-button size="small" type="error" ghost @click="showDeleteProvider = true">
            {{ t('providers.deleteProvider') }}
          </n-button>
        </template>

        <ResponsiveDropdown
          v-else
          trigger="click"
          placement="bottom-end"
          :trigger-text="t('common.actions')"
          :loading="testingAll"
          :options="headerActionOptions"
          @select="onHeaderAction"
        />
      </template>
    </PageHeader>

    <n-tabs v-model:value="activeTab" type="line" animated>
      <n-tab-pane name="keys" :tab="t('providers.tabKeys')">
        <div class="keys-toolbar">
          <span v-if="pendingCount !== null" class="keys-toolbar__count">
            {{ t('providers.testAllPendingCount', { count: pendingCount }) }}
          </span>
          <n-space v-if="!isMobile">
            <n-button @click="showAddKey = true">
              <template #icon><Plus :size="16" /></template>
              {{ t('providers.addKey') }}
            </n-button>
            <n-button type="primary" :loading="testingAll" @click="onTestAll">
              <template #icon><PlayCircle :size="16" /></template>
              {{ t('providers.testAllButton') }}
            </n-button>
          </n-space>
        </div>

        <div class="data-table-wrapper">
          <ResponsiveDataTable
            :columns="keyColumns"
            :data="provider.keys"
            :loading="testingAll"
            :scroll-x="930"
            :row-key="(row: ProviderKey) => row.id"
            :pagination="keysPagination"
          />
        </div>

        <n-alert v-if="batchSummary" type="info" class="summary">{{ batchSummary }}</n-alert>
      </n-tab-pane>

      <n-tab-pane name="models" :tab="t('providers.tabModels')">
        <div v-if="!isMobile" class="models-toolbar">
          <n-button
            type="error"
            ghost
            :disabled="checkedCandidateIds.length === 0 || batchDeleting"
            :loading="batchDeleting"
            @click="onBatchDelete"
          >
            <template #icon><Trash2 :size="16" /></template>
            {{ batchDeleteButtonLabel }}
          </n-button>
          <n-button class="ml-4" type="primary" @click="openImportModels">
            <template #icon><CloudDownload :size="16" /></template>
            {{ t('models.importModelsButton') }}
          </n-button>
        </div>
        <EmptyState v-if="candidatesError" :title="t('common.networkError')" />
        <EmptyState v-else-if="!candidatesLoading && candidateRows.length === 0" :title="t('providers.modelsEmpty')" />
        <!-- The shared mapping table: filter bar, columns, and row actions all
             live in CandidateTable; this page injects its row-identity
             adapter, its own column groups, the probe badge column (its rows
             carry the probe-queue fields), and the action handlers. -->
        <CandidateTable
          v-else
          v-model:checked-row-keys="checkedCandidateIds"
          :rows="candidateRows"
          :loading="candidatesLoading"
          :pagination="modelsPagination"
          :row-ids="candidateRowIds"
          :name-fields="candidateNameFields"
          :search-placeholder="`${t('models.name')} / ${t('models.providerModelName')}`"
          :empty-title="t('providers.modelsEmpty')"
          :scroll-x="972"
          :price-column="candidatePriceColumn"
          :leading-columns="candidateLeadingColumns"
          show-probe-state-column
          :trailing-columns="candidateTrailingColumns"
          :busy-row-id="retestAction.activeId.value"
          :retestable="retestableCandidate"
          disable-retest-while-busy
          @edit="onEditCandidate"
          @retest="onRetestCandidate"
          @delete="onDeleteCandidate"
        />
        <n-alert v-if="batchDeleteFailedSummary" type="warning" class="summary">{{ batchDeleteFailedSummary }}</n-alert>
      </n-tab-pane>
    </n-tabs>

    <KeyEditModal
      v-model:show="showAddKey"
      :provider-id="provider.id"
      :base-url="provider.base_url"
      :provider-type="provider.provider_type"
      :destination-count="destinationCount"
      :protocol-endpoints="provider.protocol_endpoints"
      @saved="reload"
    />
    <KeyEditModal
      v-model:show="showEditKey"
      :provider-id="provider.id"
      :base-url="provider.base_url"
      :provider-type="provider.provider_type"
      :destination-count="destinationCount"
      :protocol-endpoints="provider.protocol_endpoints"
      :editing-key="editingKey"
      @saved="onKeySaved"
    />
    <ProviderEditModal v-model:show="showEditProvider" :provider="provider" @updated="reload" />
    <ImportModelsModal v-model:show="showImportModels" :provider-id="provider.id" @imported="onImported" />
    <!-- The shared candidate edit modal the model page uses, opened on this
         provider's mapping row. model-id comes from the ROW (each row belongs
         to its own outward model) and drives the modal's PATCH URL. -->
    <CandidateEditModal
      v-model:show="showEditCandidate"
      :model-id="editingRow?.model_id ?? 0"
      :model-name="editingRow?.model_name ?? ''"
      :editing-candidate="editableCandidate"
      @saved="onCandidateEdited"
      @retest="onModalRetest"
    />
    <DeleteProviderModal v-model:show="showDeleteProvider" :provider="provider" @deleted="onProviderDeleted" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NSpace, NSwitch, NTag, useDialog, useMessage, type DataTableColumns } from 'naive-ui'
import { ChevronDown, ChevronUp, CloudDownload, MoreHorizontal, Plus, PlayCircle, Trash2 } from '@lucide/vue'
import { useProvidersStore } from '../../store/providers'
import { listProviderCandidates, retestCandidate, deleteCandidate, getModel, type ImportProviderModelsResult, type ModelCandidate, type ProviderCandidate } from '../../api/models'
import { candidateIsOwedWork, candidateProgressState, PROGRESS_POLL_BACKOFF_CAP_MS, PROGRESS_POLL_BASE_MS, summarizeImportProgress } from '../../utils/importProgress'
import { BATCH_DELETE_CONCURRENCY, runWithLimit } from '../../utils/runWithLimit'
import { renderFailReasonCell } from '../../utils/probeStateTag'
import { displayMessage } from '../../api/client'
import { copyToClipboard } from '../../utils/clipboard'
import { useConfirmedStatusToggle } from '../../composables/useConfirmedStatusToggle'
import { providerDisableCopy } from '../../utils/impactSummary'
import type { BatchTestResult, KeyTestTarget, Provider, ProviderKey } from '../../api/providers'
import { revealProviderKey } from '../../api/providers'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import KeyEditModal from '../../components/providers/KeyEditModal.vue'
import ImportModelsModal from '../../components/models/ImportModelsModal.vue'
import CandidateEditModal from '../../components/models/CandidateEditModal.vue'
import ProviderEditModal from '../../components/providers/ProviderEditModal.vue'
import DeleteProviderModal from '../../components/providers/DeleteProviderModal.vue'
import KeyTestTargetsTag from '../../components/providers/KeyTestTargetsTag.vue'
import ProviderProxyWarning from '../../components/providers/ProviderProxyWarning.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import ResponsiveDropdown from '../../components/common/ResponsiveDropdown.vue'
import CandidateTable, { type CandidatePriceColumn, type CandidateRowIds } from '../../components/models/CandidateTable.vue'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { candidateTestResultText } from '../../utils/modelStatusDisplay'
import { redirectIfSessionExpired } from '../../utils/sessionExpiredRedirect'
import { isTestSuccess, testOutcomeI18nKey, testOutcomeLabel, TEST_OUTCOME_MODEL_NOT_FOUND, TEST_OUTCOME_UPSTREAM_ERROR } from '../../utils/testOutcomeDisplay'
import { hintTag } from '../../utils/hintTag'
import { hasKeyTestBreakdown, passedBreakdownVisible } from '../../utils/keyTestTargets'
import { verificationDestinationCount } from '../../utils/providerProtocol'
import { deleteLeavesProviderUnusable, isLastUsableKey } from '../../utils/providerStatusDisplay'
import { useSingleRowAction } from '../../composables/useSingleRowAction'
import { useClientPagination } from '../../composables/useClientPagination'
import { useGlobalProxyMode } from '../../composables/useGlobalProxyMode'
import { providerProxyEffective } from '../../utils/outboundProxy'
import { useIsMobile } from '../../composables/useIsMobile'
import { useBackoffPoll } from '../../composables/useBackoffPoll'

const { t, te } = useI18n()
const route = useRoute()
const router = useRouter()
const dialog = useDialog()
const toggleStatusWithConfirm = useConfirmedStatusToggle(dialog)
const message = useMessage()
const store = useProvidersStore()
const isMobile = useIsMobile()

// Independent client-side pagination for the two tables on this page — both are
// fully-fetched admin lists that can grow long (many keys, or many models
// mapped to one provider).
const {
  pagination: keysPagination,
} = useClientPagination()
const {
  pagination: modelsPagination,
} = useClientPagination()

const providerId = Number(route.params.id)
const provider = ref<Provider | null>(null)
// The global outbound-proxy stance resolves this provider's warning icon
// when its own stance inherits; fetched once per mount, fail-open to off.
const { mode: globalProxyMode, fetchGlobalMode } = useGlobalProxyMode()
const providerProxied = computed(() =>
  provider.value !== null && providerProxyEffective(provider.value.proxy_mode, globalProxyMode.value),
)
const activeTab = ref('keys')
const showAddKey = ref(false)
const showEditKey = ref(false)
const editingKey = ref<ProviderKey | null>(null)
const showEditProvider = ref(false)
const showImportModels = ref(false)
const showDeleteProvider = ref(false)

// The page's subject no longer exists after a delete — leave for the list.
function onProviderDeleted() {
  void router.push('/providers')
}

// Every way of opening the import dialog also lands the page on the Models
// tab: the mobile header action and the first-setup handoff can fire while
// the Keys tab is active, and closing the dialog would otherwise drop the
// user back on Keys instead of the freshly imported rows.
function openImportModels() {
  activeTab.value = 'models'
  showImportModels.value = true
}
const testingAll = ref(false)
// Tracks the single key currently running its own "Test Connection" (distinct from
// testingAll's batch run) so the actions button can show a spinner instead
// of silently doing nothing until the request resolves.
const testingKeyId = ref<number | null>(null)
// Single-flight reveal-and-copy: the tracked value is deliberately an ID,
// never the plaintext — reveal's contract is copy-only, so nothing about
// the fetched key may survive the handler's scope.
const copyAction = useSingleRowAction()
// Single-flight reorder: only one key can be moving at a time, and the
// clicked arrow shows a spinner (activeId + direction) while it runs.
const reorderAction = useSingleRowAction()
const batchSummary = ref('')
// Keyed by provider_key.id — populated once per completed batch test,
// cleared at the start of the next one (see onTestAll). Rendered as a
// per-key badge in the template above.
const batchResultByKeyId = ref<Record<number, BatchTestResult>>({})

// "N keys pending" = the keys batch test will actually hit. Batch test
// covers every key that isn't awaiting re-entry (regardless of enabled
// status — a fresh key is disabled until it passes), so this count must
// match that scope, not just the enabled subset.
const pendingCount = computed(() => {
  if (!provider.value) return null
  return provider.value.keys.filter((k) => !k.needs_reentry).length
})

// How many destinations one key test walks server-side. It multiplies the
// request budget, so a two-endpoint provider is not cut off halfway.
const destinationCount = computed(() =>
  provider.value ? verificationDestinationCount(provider.value.provider_type, provider.value.protocol_endpoints) : 1,
)

// On mobile the header buttons and the keys-toolbar buttons collapse into a
// single ResponsiveDropdown, so the toggle-status row's label follows the
// provider's current management_status and Test All disables while a run is in
// flight (the trigger button also spins via :loading="testingAll").
const headerActionOptions = computed(() => [
  { label: t('providers.editProvider'), key: 'edit' },
  { label: t('costs.detail.viewCost'), key: 'viewCost' },
  { label: t('providers.addKey'), key: 'addKey' },
  { label: t('models.importModelsButton'), key: 'importModels' },
  { label: t('providers.testAllButton'), key: 'testAll', disabled: testingAll.value },
  {
    label: provider.value?.management_status === 1 ? t('providers.statusDisabled') : t('providers.statusEnabled'),
    key: 'toggleStatus',
  },
  { label: t('providers.deleteProvider'), key: 'delete', props: { style: 'color: var(--color-danger)' } },
])

function onHeaderAction(key: string) {
  if (key === 'edit') showEditProvider.value = true
  else if (key === 'viewCost') router.push(`/costs/providers/${provider.value!.id}`)
  else if (key === 'addKey') showAddKey.value = true
  else if (key === 'importModels') openImportModels()
  else if (key === 'testAll') void onTestAll()
  else if (key === 'toggleStatus') onToggleProviderStatus()
  else if (key === 'delete') showDeleteProvider.value = true
}

function batchResultLabel(result: BatchTestResult): string {
  if (result.needs_reentry) return t('providers.needsReentry')
  // Must precede the skipped branch: a not-run key is skipped too, and
  // "test failed" would be a verdict on a credential nothing tried.
  if (result.not_run) return t('providers.notRun')
  if (result.skipped || result.outcome === null) return t('providers.testFailed')
  return testOutcomeLabel(t, result.outcome) + ` (${result.duration_ms}ms)`
}

function batchResultTagType(result: BatchTestResult): 'success' | 'warning' | 'error' {
  if (result.needs_reentry || result.skipped) return 'warning'
  return result.outcome === 0 ? 'success' : 'error'
}

// Some category hints are written for the test-connection dialog and direct
// the operator at controls that only exist there; those categories get
// row-specific copy instead. Both maps are keyed by outcome int, not by
// derived i18n key — the key lookup falls back for unknown values and would
// silently extend an override to categories it was never written for. They
// are split by WHY the dialog copy fails on a row, because the reasons stop
// applying under different conditions:
//
// The dialog hint points at the dialog's expandable raw-error panel. A row
// whose breakdown is one click away has the equivalent, so the override is
// only needed when the row carries no breakdown.
const NO_PANEL_HINT_OVERRIDES: Record<number, string> = {
  [TEST_OUTCOME_UPSTREAM_ERROR]: 'providers.outcomeUpstreamError_rowHint',
}
// The dialog hint points at a control that exists only in the key dialog
// (the "Fetch models" button). No amount of breakdown gives the row that
// control, so the override always applies.
const MISSING_CONTROL_HINT_OVERRIDES: Record<number, string> = {
  [TEST_OUTCOME_MODEL_NOT_FOUND]: 'providers.outcomeModelNotFound_rowHint',
}

// Shorthand for the disclosure-tag component so the render sites below stay
// one call wide. The component owns the open/closed state (aria-expanded) and
// the lazily built rows.
function testTargetsTag(
  text: string,
  type: 'success' | 'warning' | 'error',
  hint: string,
  targets: KeyTestTarget[] | null | undefined,
) {
  return h(KeyTestTargetsTag, { text, type, hint, targets: targets ?? null })
}

// The stored outcome category of the key's last test, rendered so the
// specific failure ("rate limited" vs "unreachable" vs "quota unavailable")
// survives a page reload instead of living only in a transient toast. Only
// non-success categories get a tag here — a passed run surfaces its
// breakdown on the verification tag itself (passedVerificationTag above). A
// needs_reentry key shows nothing either: its stored result was authorized
// against a superseded destination and presenting it as current would
// mislead.
function lastTestResultTag(row: ProviderKey) {
  if (row.last_test_result === null || isTestSuccess(row.last_test_result) || row.needs_reentry) return null
  const label = testOutcomeLabel(t, row.last_test_result)
  const hasBreakdown = hasKeyTestBreakdown(row.last_test_targets)
  const baseHintKey = `providers.${testOutcomeI18nKey(row.last_test_result)}_hint`
  const hintKey =
    MISSING_CONTROL_HINT_OVERRIDES[row.last_test_result] ??
    (hasBreakdown ? baseHintKey : NO_PANEL_HINT_OVERRIDES[row.last_test_result] ?? baseHintKey)
  // Gated on te(): a future category without a hint must degrade to the bare
  // label, not read its raw message key to a screen reader.
  const hint = te(hintKey) ? t(hintKey) : ''
  if (!hasBreakdown) return hintTag({ text: label, type: 'warning', hint })
  return testTargetsTag(label, 'warning', hint, row.last_test_targets)
}

onMounted(async () => {
  try {
    await reload()
  } catch (err) {
    message.error(displayMessage(err, t))
    return
  }
  // Resolves the header's proxy warning for an inheriting stance; a failure
  // only means no icon (fail-open to off), so it stays silent.
  void fetchGlobalMode()
  await loadCandidates()
  resumeQueuePollingFromLoadedRows()
  // First-setup handoff: the create flow parked this provider's id in the
  // store so the import dialog opens by itself, once. In-memory on purpose —
  // a refresh must not re-open the dialog, and a URL flag would remount the
  // page (DefaultLayout keys its router-view by fullPath) and wipe it.
  if (store.pendingImportProviderId === providerId) {
    store.pendingImportProviderId = null
    openImportModels()
  }
})

async function reload() {
  provider.value = await store.fetchDetail(providerId)
}

// The mappings this provider serves, with verification state and failure
// reason — the provider-scoped list endpoint the import progress view also
// polls.
const candidateRows = ref<ProviderCandidate[]>([])
const candidatesLoading = ref(false)
const candidatesError = ref(false)
// Race guard token (same pattern as the shared stores): loadCandidates has
// several triggers (mount, import finished, dialog closed, retest) that can
// overlap, and a slow stale response must not overwrite a newer one.
let candidatesFetchId = 0

// Search + verification-result filtering for the mapping table lives in the
// shared CandidateTable (it wires the shared composable itself); this page
// only names the two columns its keyword searches.
function candidateNameFields(row: ProviderCandidate): string[] {
  return [row.model_name, row.provider_model_name]
}

// The row-identity adapter for the shared table: the provider page's mapping
// rows speak the provider-scoped list endpoint's vocabulary — candidate_id +
// model_id — so every row action this page receives resolves through these.
function candidateRowIds(row: ProviderCandidate): CandidateRowIds {
  return { candidateId: row.candidate_id, modelId: row.model_id }
}

// silent skips the table's loading spinner and keeps failures quiet —
// background polls fire every couple of seconds and must neither blink the
// table nor stack an error toast per tick; the poll retries off the returned
// outcome instead. 'stale' means a newer fetch superseded this one and nothing
// was written; 'expired' means the session lapsed and we are on our way to the
// login page — callers must stop polling, not retry.
async function loadCandidates(silent = false): Promise<'ok' | 'stale' | 'error' | 'expired'> {
  const fetchId = ++candidatesFetchId
  if (!silent) {
    candidatesLoading.value = true
    candidatesError.value = false
  } else {
    // Taking over from an in-flight visible fetch supersedes it: its finally
    // is stale-guarded and can no longer clear the spinner, and a silent
    // refresh is by definition not visibly loading — so clear it here, or the
    // table spins forever after "import → close dialog immediately".
    candidatesLoading.value = false
  }
  try {
    const { list } = await listProviderCandidates(providerId)
    if (fetchId !== candidatesFetchId) return 'stale'
    candidateRows.value = list
    candidatesError.value = false
    return 'ok'
  } catch (err) {
    // Before the stale guard on purpose: a superseded fetch that hit session
    // expiry is still a session expiry, and swallowing it as "stale" would
    // leave the dead session polling forever.
    if (redirectIfSessionExpired(err, router)) return 'expired'
    if (fetchId !== candidatesFetchId) return 'stale'
    if (!silent) {
      candidatesError.value = true
      message.error(displayMessage(err, t))
    }
    return 'error'
  } finally {
    if (fetchId === candidatesFetchId && !silent) candidatesLoading.value = false
  }
}

// A finished import created models and mappings this page shows. The ids it
// stored are kept so the tab can keep refreshing until every one has settled.
// If the result arrives after the dialog is already gone (it was dismissed or
// unmounted while the request was in flight), the close watcher has long since
// run with the OLD ids — so the takeover polling starts here instead.
function onImported(result: ImportProviderModelsResult) {
  // Merged, not replaced: a second import can finish while an earlier batch's
  // probes are still running, and dropping the earlier ids would let polling
  // declare "done" on the new batch alone — freezing the old rows as pending.
  // Ids already settled contribute nothing to the done condition, so the
  // union stays correct.
  const ids = new Set(importedIds)
  for (const item of result.items) {
    if (item.candidate_id) ids.add(item.candidate_id)
  }
  importedIds = [...ids]
  void reload().catch((err) => message.error(displayMessage(err, t)))
  if (!showImportModels.value && importedIds.length > 0) {
    pollCandidatesUntilSettled()
    return
  }
  void loadCandidates()
}

// Mappings a bulk import stored whose probes may still be running — the tab
// polls these to their terminal states after the dialog closes, because the
// close-anytime flow promises the probing continues without it.
let importedIds: number[] = []
// The shared generation-guarded backoff loop; see useBackoffPoll.
const candidatesPoll = useBackoffPoll(PROGRESS_POLL_BASE_MS, PROGRESS_POLL_BACKOFF_CAP_MS)

function pollCandidatesUntilSettled() {
  candidatesPoll.start(async (isCurrent) => {
    // Row writes are governed by loadCandidates' own fetch token (a newer
    // fetch supersedes an older one); this guard covers the LOOP's decisions
    // — a tick that outlived its polling generation must neither declare the
    // batch done nor adjust the pacing.
    const outcome = await loadCandidates(true)
    if (!isCurrent()) return 'stop'
    if (outcome === 'expired') return 'stop'
    // Only a FRESH response may declare the batch settled: a failed poll
    // leaves the rows from before the import in place, and their missing
    // imported ids would read as "all done" while probes are still running.
    if (outcome === 'ok') {
      return summarizeImportProgress(candidateRows.value, importedIds).progress.done ? 'done' : 'again'
    }
    // 'stale' means a newer fetch superseded this one — keep the base pace.
    return outcome === 'error' ? 'error' : 'again'
  })
}

// While the import dialog is open it does its own polling; when it closes the
// tab takes over, refreshing until the imported rows all hold terminal states
// — closing early must not freeze the tab on "queued" until a manual reload.
// With nothing imported there is nothing to wait for: one refresh, no loop.
watch(showImportModels, (open) => {
  candidatesPoll.stop()
  if (open) return
  if (importedIds.length === 0) {
    void loadCandidates()
    return
  }
  pollCandidatesUntilSettled()
})

// A page opened (or refreshed) while the probe queue is already working this
// provider's mappings must keep refreshing them: the rows render as queued or
// probing, but the import dialog that started those probes — and its polling
// handoff — lived in a previous page instance. Adopt the busy rows as the ids
// to watch and poll them to their terminal states, exactly as the dialog-close
// handoff would have; otherwise finished probes stay displayed as pending
// until a manual reload.
function resumeQueuePollingFromLoadedRows() {
  if (importedIds.length > 0) return
  // queue_state is process-local: in a multi-instance deployment the probes
  // may be running on an instance this request never reached, and every
  // queue_state comes back empty. The durable signal is the row itself — an
  // untested, unstamped row still carrying the auto-enable promise is owed a
  // probe outcome (the server re-enqueues exactly these rows at startup), so
  // those rows are adopted for polling too. Every such row settles: a
  // verdict, an abandonment stamp, or a recovery probe ends the wait. Rows
  // WITHOUT the promise were stored unprobed on purpose (a manual save-as-
  // disabled) — polling them would wait on a probe nobody owes.
  const busy = candidateRows.value.filter(candidateIsOwedWork).map((row) => row.candidate_id)
  if (busy.length === 0) return
  importedIds = busy
  candidatesPoll.stop()
  pollCandidatesUntilSettled()
}

// Single-flight retest: one mapping at a time, spinner on the clicked row.
const retestAction = useSingleRowAction()

async function onRetestCandidate(row: ProviderCandidate) {
  await retestAction.run(row.candidate_id, async () => {
    try {
      const { candidate: updated, applied } = await retestCandidate(row.model_id, row.candidate_id)
      await loadCandidates()
      if (!applied) {
        // A concurrent probe won the commit race, so the row now carries ITS
        // result; announcing it as this click's could state the opposite of
        // what this run observed.
        message.info(t('providers.retestSuperseded'))
        return
      }
      // Judged on last_test_result — THIS run's basic-probe outcome — not on
      // verification_status: an inconclusive run (rate limited, unreachable)
      // deliberately leaves a previously decisive verdict alone, so reading
      // the status would replay the old verdict as this run's result. Same
      // rule and wording as the model detail page's retest.
      const passed = updated.last_test_result !== null && isTestSuccess(updated.last_test_result)
      message[passed ? 'success' : 'warning'](candidateTestResultText(t, passed, updated.last_test_result))
    } catch (err) {
      if (redirectIfSessionExpired(err, router)) return
      message.error(displayMessage(err, t))
    }
  })
}

// ---- Inline candidate edit / delete (the models tab's actions column) ----

// The mapping row currently open in the shared candidate edit modal, kept in
// this page's own ProviderCandidate shape. The modal speaks the model page's
// ModelCandidate vocabulary, so the row is adapted for it below; model_id —
// which the modal needs for its PATCH URL but ModelCandidate does not carry —
// is passed straight from the row (see the template's CandidateEditModal).
const showEditCandidate = ref(false)
const editingRow = ref<ProviderCandidate | null>(null)

// Only the pricing/identity fields below are backfilled by the modal; the
// model-page-only slots (capability flags, binding_count, sort_order, …) get
// inert values because nothing in the modal reads them.
const editableCandidate = computed<ModelCandidate | null>(() => {
  const row = editingRow.value
  if (!row) return null
  return {
    id: row.candidate_id,
    provider_id: providerId,
    provider_name: provider.value?.name ?? '',
    provider_model_name: row.provider_model_name,
    input_price: row.input_price,
    output_price: row.output_price,
    cache_write_price: row.cache_write_price,
    cache_read_price: row.cache_read_price,
    max_output: row.max_output,
    billing_mode: row.billing_mode,
    image_pricing_tiers: row.image_pricing_tiers,
    video_pricing_tiers: row.video_pricing_tiers,
    audio_unit_price: row.audio_unit_price,
    supports_streaming: null,
    supports_function_calling: null,
    management_status: row.management_status,
    sort_order: 1,
    verification_status: row.verification_status,
    routable: false,
    blocked_by: '',
    last_test_result: row.last_test_result,
    last_test_duration_ms: null,
    last_tested_at: row.last_tested_at,
    binding_count: 0,
  }
})

function onEditCandidate(row: ProviderCandidate) {
  editingRow.value = row
  showEditCandidate.value = true
}

// A saved edit rewrote this provider's mapping (price, upstream name, …) —
// the rows refresh so the table shows the stored values.
function onCandidateEdited() {
  void loadCandidates()
}

// The "saved without a retest" toast offers one; the emit carries only the
// candidate id while this page's retest also needs the row's model_id.
function onModalRetest(candidateId: number) {
  const row = candidateRows.value.find((r) => r.candidate_id === candidateId)
  if (row) void onRetestCandidate(row)
}

// Deleting a model's last routable candidate on this provider takes the
// provider off that model entirely — expected behavior, but said out loud
// before the confirm. This page's rows carry no routable flag of their own,
// so the model detail endpoint is prefetched before the dialog opens and the
// remaining-routable count is computed exactly as the model page does it
// (candidates' routable field, minus the row being deleted).
async function onDeleteCandidate(row: ProviderCandidate) {
  let lastRoutable = false
  try {
    const model = await getModel(row.model_id)
    const rowRoutable = model.candidates.some((c) => c.id === row.candidate_id && c.routable)
    const othersRoutable = model.candidates.some((c) => c.routable && c.id !== row.candidate_id)
    lastRoutable = rowRoutable && !othersRoutable
  } catch (err) {
    if (redirectIfSessionExpired(err, router)) return
    // The guard is advisory copy, not a precondition: a failed prefetch
    // downgrades to the plain confirm instead of blocking a delete the
    // backend would accept.
  }
  // A blank provider model name means "use the model's own name" — shown
  // that way rather than as empty brackets.
  const label = row.provider_model_name || row.model_name
  dialog.warning({
    title: t('models.confirmDeleteCandidateTitle'),
    content: lastRoutable
      ? t('providers.confirmDeleteCandidateLastRoutableContent', { model: row.model_name, name: label })
      : t('providers.confirmDeleteCandidateContent', { model: row.model_name, name: label }),
    positiveText: t('common.delete'),
    negativeText: t('providers.cancel'),
    onPositiveClick: async () => {
      try {
        await deleteCandidate(row.model_id, row.candidate_id)
        await loadCandidates()
      } catch (err) {
        if (redirectIfSessionExpired(err, router)) return
        message.error(displayMessage(err, t))
      }
    },
  })
}

// ---- Batch delete (the models tab's selection column + toolbar button) ----

// Checked rows of the mapping table, as candidate ids (the table's row-key).
// Desktop only: the mobile card layout has no checkboxes, so the toolbar
// button (hidden on mobile) and the selection column (gated by isMobile in
// candidateLeadingColumns) travel together.
const checkedCandidateIds = ref<Array<string | number>>([])
const batchDeleting = ref(false)
// The partial-failure summary shown under the table. Empty (no alert) after a
// clean batch — only failures land here.
const batchDeleteFailedSummary = ref('')

const batchDeleteButtonLabel = computed(() =>
  checkedCandidateIds.value.length === 0
    ? t('providers.batchDeleteButton')
    : t('providers.batchDeleteButtonWithCount', { count: checkedCandidateIds.value.length }),
)

// A failed row is named by the same pair everywhere a row is spoken about:
// model name plus the upstream name (blank means "the model's own name").
function candidateRowLabel(row: ProviderCandidate): string {
  return `${row.model_name} (${row.provider_model_name || row.model_name})`
}

// The batch form of the single-row guard: one getModel per AFFECTED model —
// several rows of the same model share the prefetch — and the remaining
// routable count subtracts the WHOLE batch for that model, so deleting a
// model's two routable candidates together is warned about, not smuggled
// through one row at a time. A failed prefetch degrades to the plain copy
// (the guard is advisory, same as the single-row delete).
async function onBatchDelete() {
  const rows = candidateRows.value.filter((r) => checkedCandidateIds.value.includes(r.candidate_id))
  if (rows.length === 0 || batchDeleting.value) return
  const rowsByModel = new Map<number, ProviderCandidate[]>()
  for (const row of rows) {
    const list = rowsByModel.get(row.model_id) ?? []
    list.push(row)
    rowsByModel.set(row.model_id, list)
  }
  const losingModels: string[] = []
  for (const [modelId, modelRows] of rowsByModel) {
    try {
      const model = await getModel(modelId)
      const deletingIds = new Set(modelRows.map((r) => r.candidate_id))
      const deletingRoutable = model.candidates.some((c) => c.routable && deletingIds.has(c.id))
      const othersRoutable = model.candidates.some((c) => c.routable && !deletingIds.has(c.id))
      if (deletingRoutable && !othersRoutable) losingModels.push(model.name)
    } catch (err) {
      if (redirectIfSessionExpired(err, router)) return
    }
  }
  dialog.warning({
    title: t('providers.confirmBatchDeleteTitle'),
    content:
      losingModels.length > 0
        ? t('providers.confirmBatchDeleteLastRoutableContent', {
            count: rows.length,
            models: losingModels.join(', '),
          })
        : t('providers.confirmBatchDeleteContent', { count: rows.length }),
    positiveText: t('common.delete'),
    negativeText: t('providers.cancel'),
    onPositiveClick: () => runBatchDelete(rows),
  })
}

// One DELETE per row through the shared concurrency limiter. Failures are
// per-row by design: the failed rows survive (the server still returns them
// on the refresh) and are summed up below, while every successful row's
// disappearance is not held hostage by the failures.
async function runBatchDelete(rows: ProviderCandidate[]) {
  batchDeleting.value = true
  batchDeleteFailedSummary.value = ''
  try {
    const outcomes = await runWithLimit(rows, BATCH_DELETE_CONCURRENCY, (row) =>
      deleteCandidate(row.model_id, row.candidate_id),
    )
    const failed = outcomes.filter((o) => !o.ok)
    for (const outcome of failed) {
      // A dead session fails every row the same way — leave for the login
      // page instead of dressing an auth expiry up as a per-row failure.
      if (redirectIfSessionExpired(outcome.error, router)) return
    }
    if (failed.length === 0) {
      message.success(t('providers.batchDeleteSucceeded', { count: rows.length }))
    } else {
      batchDeleteFailedSummary.value =
        t('providers.batchDeleteFailedSummary', {
          passed: rows.length - failed.length,
          failed: failed.length,
        }) +
        ' ' +
        t('providers.batchDeleteFailedRows', {
          rows: failed.map((o) => candidateRowLabel(o.item)).join(', '),
        })
    }
    await loadCandidates()
  } finally {
    // The selection never outlives its batch: the ids it references are gone
    // or freshly refreshed, and the next batch must be an explicit choice.
    checkedCandidateIds.value = []
    batchDeleting.value = false
  }
}

// ---- The models tab's shared-table injections ----

// Retest stays conditional, exactly as before the shared-table refactor:
// anything without a passing verdict that the probe queue does not already
// hold (failed rows and the "untested" leftovers of an interrupted queue
// alike); rows the queue holds get no retest because their probe is already
// coming.
function retestableCandidate(row: ProviderCandidate): boolean {
  return (
    candidateProgressState(row) !== 'passed' &&
    row.queue_state !== 'queued' &&
    row.queue_state !== 'probing'
  )
}

// This page's own columns around the shared table's core. Leading: the batch
// selection column (desktop only — the mobile card layout cannot render
// checkboxes and the toolbar batch button is desktop-only with it) plus the
// plain model name. Trailing: the probe failure reason (the probe badge
// column itself renders inside the shared table; these rows carry the queue
// fields it reads). The unpriced badge lives inside the shared table too, on
// the provider-model-name column — one renderer for both pages.
// The price column shows no mode tag — the mapping table is already
// per-billing-mode context here.
const candidatePriceColumn = computed<CandidatePriceColumn>(() => ({
  title: t('providers.candidatePrice'),
  tip: t('providers.candidatePrice_tip'),
  minWidth: 120,
}))
const candidateLeadingColumns = computed<DataTableColumns<ProviderCandidate>>(() => [
  ...(!isMobile.value ? [{ type: 'selection' } as const] : []),
  {
    title: columnTitle(t('models.name'), t('models.name_tip')),
    key: 'model_name',
    minWidth: 180,
    render: (row) => row.model_name,
  },
])

const candidateTrailingColumns = computed<DataTableColumns<ProviderCandidate>>(() => [
  {
    title: columnTitle(t('providers.candidateFailReason'), t('providers.candidateFailReason_tip')),
    key: 'last_test_error',
    minWidth: 200,
    render: renderFailReasonCell,
  },
])

function verificationLabel(status: number): string {
  if (status === 1) return t('providers.verificationPassed')
  if (status === 2) return t('providers.verificationFailed')
  return t('providers.verificationUntested')
}

function verificationTagType(status: number): 'success' | 'error' | 'default' {
  if (status === 1) return 'success'
  if (status === 2) return 'error'
  return 'default'
}

// A passed key's breakdown entry. Failures carry their own clickable result
// tag, but a key whose last run passed shows only the verification tag — so
// that tag itself opens the per-protocol panel when the run recorded one,
// letting an operator see that every configured destination passed, not just
// the aggregate. Returns null when the stored run no longer speaks for the
// key (see passedBreakdownVisible), and the caller keeps the plain tag.
function passedVerificationTag(row: ProviderKey) {
  if (!passedBreakdownVisible(row)) return null
  // Type is fixed at 'success': the predicate requires verification passed.
  return testTargetsTag(verificationLabel(row.verification_status), 'success', '', row.last_test_targets)
}

// A real NDataTable with defined columns rather than a hand-rolled list of
// flex rows — kept as a computed so the columns re-render when the active
// locale or batchResultByKeyId changes.
const keyColumns = computed<DataTableColumns<ProviderKey>>(() => [
  { title: columnTitle(t('providers.keyLabel'), t('providers.keyLabel_tip')), key: 'label', minWidth: 140 },
  {
    title: columnTitle(t('providers.keyPlaintext'), t('providers.keyPlaintext_tip')),
    key: 'key_prefix',
    minWidth: 140,
    // Prefix + last-4 suffix is the display pair that tells near-identical
    // keys apart. Rows created before the suffix column existed (and not
    // yet derived by the startup backfill) carry '' — those degrade to the
    // prefix-only form rather than showing a blank or a dangling separator.
    render: (row) =>
      h(
        'span',
        { class: 'key-prefix-cell' },
        row.key_suffix ? `${row.key_prefix}***${row.key_suffix}` : `${row.key_prefix}***`,
      ),
  },
  {
    title: columnTitle(t('providers.testModel'), t('providers.testModel_tip')),
    key: 'test_model',
    minWidth: 140,
  },
  {
    title: columnTitle(t('providers.statusColumn'), t('providers.statusColumn_tip')),
    key: 'status',
    minWidth: 220,
    render: (row) => {
      const tags = [
        passedVerificationTag(row) ??
          h(
            NTag,
            { size: 'small', bordered: false, type: verificationTagType(row.verification_status) },
            { default: () => verificationLabel(row.verification_status) },
          ),
      ]
      if (row.needs_reentry) {
        tags.push(
          h(NTag, { type: 'warning', size: 'small', bordered: false }, { default: () => t('providers.needsReentry') }),
        )
      }
      const batchResult = batchResultByKeyId.value[row.id]
      if (!batchResult) {
        // The fresh in-session batch verdict outranks the stored one — the
        // row's last_test_result is stale until the next reload.
        const stored = lastTestResultTag(row)
        if (stored) tags.push(stored)
      } else {
        const tagType = batchResultTagType(batchResult)
        tags.push(
          // The breakdown is read off the batch response rather than the
          // reloaded key row: a probe whose write lost a CAS race still
          // reported what each destination said, while the row it failed to
          // update still describes an older run. Entries that probed nothing
          // (needs_reentry, not_run) carry no targets and fail this check on
          // their own.
          hasKeyTestBreakdown(batchResult.last_test_targets)
            ? testTargetsTag(batchResultLabel(batchResult), tagType, '', batchResult.last_test_targets)
            : h(
                NTag,
                { type: tagType, size: 'small', bordered: false },
                { default: () => batchResultLabel(batchResult) },
              ),
        )
      }
      return h(NSpace, { size: 4 }, { default: () => tags })
    },
  },
  {
    title: columnTitle(t('providers.managementStatusColumn'), t('providers.managementStatusColumn_tip')),
    key: 'management_status',
    width: STATUS_COL_WIDTH,
    align: 'center',
    render: (row) =>
      h(NSwitch, {
        value: row.management_status === 1,
        'onUpdate:value': (v: boolean) => onToggleKeyStatus(row.id, v),
      }),
  },
  {
    title: t('providers.reorderColumn'),
    key: 'reorder',
    width: 70,
    align: 'center',
    render: (row, index) => {
      const count = provider.value?.keys.length ?? 0
      const active = reorderAction.activeId.value
      const reordering = active !== null
      const upLoading = active === row.id && reorderAction.direction.value === 'up'
      const downLoading = active === row.id && reorderAction.direction.value === 'down'
      return h('div', { style: 'display:inline-flex;align-items:center;gap:2px;justify-content:center' }, [
        h(
          NButton,
          { size: 'small', quaternary: true, circle: true, disabled: reordering || index === 0, loading: upLoading, title: t('providers.moveUp'), onClick: () => onReorder(row.id, 'up') },
          { icon: () => h(ChevronUp, { size: 16 }) },
        ),
        h(
          NButton,
          { size: 'small', quaternary: true, circle: true, disabled: reordering || index >= count - 1, loading: downLoading, title: t('providers.moveDown'), onClick: () => onReorder(row.id, 'down') },
          { icon: () => h(ChevronDown, { size: 16 }) },
        ),
      ])
    },
  },
  {
    // Actions-column convention: a single compact "···" dropdown rather
    // than several inline text buttons — the inline-button version made
    // this column wide enough to force the whole table into horizontal
    // scroll.
    title: t('common.actions'),
    key: 'actions',
    width: 60,
    align: 'center',
    render: (row) =>
      h(
        ResponsiveDropdown,
        {
          trigger: 'click',
          placement: 'bottom-end',
          triggerText: t('common.actions'),
          disabled: testingKeyId.value === row.id,
          loading: testingKeyId.value === row.id,
          height: 200,
          options: [
            { label: t('providers.editKey'), key: 'edit' },
            { label: t('providers.testConnection'), key: 'test', disabled: row.needs_reentry },
            { label: t('common.copy'), key: 'copy' },
            { type: 'divider', key: 'd' },
            { label: t('providers.deleteKey'), key: 'delete', props: { style: 'color: var(--color-danger)' } },
          ],
          onSelect: (key: string) => {
            if (key === 'edit') onEditKey(row)
            else if (key === 'test') onTestOneKey(row.id)
            else if (key === 'copy') void onCopyKey(row)
            else if (key === 'delete') onDeleteKey(row)
          },
        },
        {
          default: () =>
            h(
              NButton,
              { size: 'small', quaternary: true, circle: true, loading: testingKeyId.value === row.id, disabled: testingKeyId.value === row.id },
              { icon: () => h(MoreHorizontal, { size: 16 }) },
            ),
        },
      ),
  },
])

function onEditKey(key: ProviderKey) {
  editingKey.value = key
  showEditKey.value = true
}

// Reveal-and-copy from the row menu: the plaintext exists in ONE local
// variable for the duration of a single clipboard write and then goes out of
// scope — never a ref, never the store, never rendered. The server rate-limits
// and audits every reveal; the in-flight guard keeps one impatient double
// click from spending two of the caller's ten-per-minute budget on one copy.
async function onCopyKey(row: ProviderKey) {
  await copyAction.run(row.id, async () => {
    try {
      const { plaintext_key } = await revealProviderKey(providerId, row.id)
      if (await copyToClipboard(plaintext_key)) message.success(t('common.copied'))
      else message.error(t('common.copyFailed'))
    } catch (err) {
      if (redirectIfSessionExpired(err, router)) return
      message.error(displayMessage(err, t))
    }
  })
}

// Deletion is final and needs no server-side precondition — any key can go,
// history rows keep their own snapshot of it. The dialog only escalates its
// copy when the provider is left with nothing usable to serve.
function onDeleteKey(row: ProviderKey) {
  const leavesUnusable = deleteLeavesProviderUnusable(provider.value?.keys ?? [], row.id)
  dialog.warning({
    title: t('providers.deleteKey'),
    content: leavesUnusable
      ? t('providers.confirmDeleteKeyLastContent', { label: row.label })
      : t('providers.confirmDeleteKeyContent', { label: row.label }),
    positiveText: t('common.delete'),
    negativeText: t('providers.cancel'),
    onPositiveClick: async () => {
      try {
        await store.deleteKey(providerId, row.id)
        // The batch panel must not keep showing a verdict for a key that no
        // longer exists.
        delete batchResultByKeyId.value[row.id]
        await reload()
      } catch (err) {
        message.error(displayMessage(err, t))
      }
    },
  })
}

// A key edit re-tests the credential server-side only when a new plaintext
// was submitted; only then is the batch run's verdict for it out of date. A
// rename-only save keeps the batch entry — evicting it would erase
// distinctions that exist nowhere else, like "skipped" or "not run".
function onKeySaved(retested: boolean) {
  if (retested && editingKey.value) delete batchResultByKeyId.value[editingKey.value.id]
  void reload()
}

async function onTestOneKey(keyId: number) {
  testingKeyId.value = keyId
  try {
    const updated = await store.testKey(providerId, keyId, destinationCount.value)
    // This key's batch verdict is now out of date, and leaving it in place
    // would keep outranking the fresh stored result in the status cell.
    delete batchResultByKeyId.value[keyId]
    await reload()
    // Two-tier feedback so the click is never silent: pass (green) vs
    // everything else (yellow) named by its specific outcome reason
    // (e.g. "unreachable"). Deliberately not mirroring the backend's
    // definitive-fail vs inconclusive split — that lives in
    // classifyTestResult and would drift if duplicated here.
    const outcome = updated.last_test_result
    if (outcome === null) return
    const label = testOutcomeLabel(t, outcome)
    if (outcome === 0) message.success(label)
    else message.warning(label)
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    testingKeyId.value = null
  }
}

async function onReorder(keyId: number, direction: 'up' | 'down') {
  await reorderAction.run(keyId, async () => {
    try {
      await store.reorderKey(providerId, keyId, direction)
      await reload()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  }, direction)
}

// isLastUsableKey (not a mere enabled-count check) decides the escalation:
// a key contributes to routing only when it's enabled AND verified AND
// needs no re-entry, so disabling the one key satisfying all three deserves
// the warning even when merely-enabled-but-unverified keys remain.
function onToggleKeyStatus(keyId: number, enable: boolean) {
  const isLastAvailable = !enable && isLastUsableKey(provider.value?.keys ?? [], keyId)
  const proceed = async () => {
    try {
      await store.setKeyStatus(providerId, keyId, enable)
      await reload()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  }
  if (isLastAvailable) {
    dialog.warning({
      title: t('providers.confirmDisableLastKeyTitle'),
      content: t('providers.confirmDisableLastKeyContent'),
      positiveText: t('providers.statusDisabled'),
      negativeText: t('providers.cancel'),
      onPositiveClick: proceed,
    })
    return
  }
  void proceed()
}

function onToggleProviderStatus() {
  if (!provider.value) return
  const enabling = provider.value.management_status !== 1
  const proceed = async () => {
    try {
      await store.setStatus(providerId, enabling)
      await reload()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  }
  toggleStatusWithConfirm(
    enabling,
    () => providerDisableCopy(providerId, t),
    proceed,
  )
}

async function onTestAll() {
  if (!provider.value) return
  testingAll.value = true
  batchSummary.value = ''
  batchResultByKeyId.value = {}
  try {
    const { results } = await store.testAll(providerId)
    // `skipped` and `outcome === 0` are not mutually exclusive: a result
    // can be both TestSuccess AND skipped (its CAS write was lost to a
    // concurrent edit — the test itself succeeded, but nothing was
    // persisted). `passed` must exclude skipped results, or a skipped+
    // successful result gets double-counted and `failed` goes negative.
    const skipped = results.filter((r) => r.skipped).length
    const passed = results.filter((r) => !r.skipped && r.outcome === 0).length
    const failed = results.length - passed - skipped
    batchSummary.value = t('providers.testAllSummary', { passed, failed, skipped })
    // Keys the run's budget never reached are the one case where the operator
    // has something to do next, so say it outright instead of leaving them to
    // infer it from the skipped count.
    const notRun = results.filter((r) => r.not_run).length
    if (notRun > 0) {
      batchSummary.value += ' ' + t('providers.testAllBudgetExhausted', { count: notRun })
    }
    batchResultByKeyId.value = Object.fromEntries(results.map((r) => [r.key_id, r]))
    await reload()
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    testingAll.value = false
  }
}
</script>

<style scoped>
.keys-toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: var(--space-4);
}

.keys-toolbar__count {
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
}

:deep(.key-prefix-cell) {
  color: var(--color-text-muted);
  font-size: var(--text-xs);
  font-family: var(--font-mono);
}

.models-toolbar {
  display: flex;
  justify-content: flex-end;
  margin-bottom: var(--space-4);
}

/* Header labels stay on one line: a wrapped three-line header (the old price
   title did this) makes the whole header row triple-height for every column.
   Long explanations belong in the "?" tooltips, not the header text. */
:deep(.n-data-table-th) {
  white-space: nowrap;
}

.summary {
  margin-top: var(--space-3);
}
</style>
