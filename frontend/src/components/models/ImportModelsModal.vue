<!-- frontend/src/components/models/ImportModelsModal.vue -->
<template>
  <!-- Wider than the 520px form dialogs on purpose: this is a batch table
       (selection + name + suggestion + four editable price columns), not a
       label/field form. -->
  <ModalDrawer
    v-model:show="showModel"
    :title="phase === 'select' ? t('models.importTitle') : t('models.importProgressTitle')"
    max-width="880px"
    :mask-closable="false"
    :close-on-esc="false"
    :confirm-text="phase === 'select' ? t('models.importSelected', { count: selectedCount }) : t('models.importClose')"
    :cancel-text="t('models.cancel')"
    :dismissable="phase === 'select' && !importing"
    :loading="importing"
    :back-label="t('common.back')"
    @confirm="onConfirm"
    @after-leave="reset"
  >
    <template v-if="phase === 'select'">
      <!-- Two different failures with two different fixes: no key to
           authenticate with (enable one), or an upstream that refused (its own
           words are shown verbatim below the category). -->
      <EmptyState
        v-if="loadFailure.kind !== 'none'"
        :icon="AlertTriangle"
        :title="loadFailure.title"
        :description="loadFailure.description"
      >
        <template v-if="loadFailure.detail" #detail>
          <pre class="upstream-detail">{{ loadFailure.detail }}</pre>
        </template>
      </EmptyState>
      <EmptyState v-else-if="!loading && rows.length === 0" :icon="Inbox" :title="t('models.importCatalogEmpty')" />
      <template v-else>
        <p class="import-hint">{{ t('models.importHint') }}</p>
        <div class="data-table-wrapper">
          <n-data-table
            size="small"
            :columns="columns"
            :data="rows"
            :loading="loading"
            :row-key="(row: ImportRow) => row.name"
            :checked-row-keys="checkedKeys"
            :max-height="420"
            @update:checked-row-keys="onCheckedKeys"
          />
        </div>
      </template>
    </template>
    <template v-else>
      <p class="import-hint">
        {{ t('models.importProgressSummary', { done: progress.total - progress.pending, total: progress.total, passed: progress.passed, failed: progress.failed, inconclusive: progress.inconclusive }) }}
      </p>
      <NProgress
        type="line"
        :percentage="progress.total ? Math.round(((progress.total - progress.pending) / progress.total) * 100) : 0"
        :show-indicator="false"
        :status="progress.failed + progress.inconclusive > 0 ? 'warning' : 'success'"
        class="import-progress-bar"
      />
      <div class="data-table-wrapper">
        <n-data-table
          size="small"
          :columns="progressColumns"
          :data="progressRows"
          :row-key="(row: ProviderCandidate) => row.candidate_id"
          :max-height="380"
        />
      </div>
      <p class="import-hint import-hint--footer">{{ t('models.importProgressHint') }}</p>
    </template>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, h, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
// NProgress is not in main.ts's create() list — it must be imported explicitly
// or Vue renders <n-progress> as an unknown element with zero build errors.
import { NInputNumber, NProgress, NTag, useMessage, type DataTableColumns } from 'naive-ui'
import { AlertTriangle, Inbox } from '@lucide/vue'
import { listModelsForProvider } from '../../api/providers'
import { importProviderModels, listProviderCandidates, suggestPrices, type ImportProviderModelsResult, type ProviderCandidate } from '../../api/models'
import { displayMessage } from '../../api/client'
import { buildImportRows, chunkByCap, IMPORT_BATCH_CAP, normalizeCatalogNames, toImportItems, type ImportRow } from '../../utils/importRows'
import { catalogueFailure, NO_CATALOGUE_FAILURE, type CatalogueFailure } from '../../utils/catalogueFailure'
import { candidateIsOwedWork, PROGRESS_POLL_BACKOFF_CAP_MS, PROGRESS_POLL_BASE_MS, summarizeImportProgress, type ImportProgress } from '../../utils/importProgress'
import { renderFailReasonCell, renderProbeStateTag } from '../../utils/probeStateTag'
import ModalDrawer from '../common/ModalDrawer.vue'
import EmptyState from '../EmptyState.vue'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { redirectIfSessionExpired } from '../../utils/sessionExpiredRedirect'
import { useBackoffPoll } from '../../composables/useBackoffPoll'

const props = defineProps<{ show: boolean; providerId: number }>()
const emit = defineEmits<{
  (e: 'update:show', v: boolean): void
  (e: 'imported', result: ImportProviderModelsResult): void
}>()

const { t } = useI18n()
const message = useMessage()
const router = useRouter()

const showModel = computed({
  get: () => props.show,
  set: (v: boolean) => emit('update:show', v),
})

const loading = ref(false)
const loadFailure = ref<CatalogueFailure>(NO_CATALOGUE_FAILURE)
const importing = ref(false)
const rows = ref<ImportRow[]>([])
const checkedKeys = ref<Array<string | number>>([])

// After a successful import the dialog switches to the progress view and
// polls the imported mappings' verification states until every one has a
// verdict — closable at any time, the probing continues server-side.
const phase = ref<'select' | 'progress'>('select')
const importedIds = ref<number[]>([])
const progressRows = ref<ProviderCandidate[]>([])
const progress = ref<ImportProgress>({ total: 0, passed: 0, failed: 0, inconclusive: 0, pending: 0, done: true })
// The shared generation-guarded backoff loop; see useBackoffPoll. dialogGeneration
// additionally invalidates the batched-submit flow when the dialog closes —
// the submit runs OUTSIDE the poll loop, so the poll's own guard cannot cover
// it.
const progressPoll = useBackoffPoll(PROGRESS_POLL_BASE_MS, PROGRESS_POLL_BACKOFF_CAP_MS)
let dialogGeneration = 0



const selectedCount = computed(() => {
  const checked = new Set(checkedKeys.value)
  return rows.value.filter((r) => (!r.added || r.unfinished) && checked.has(r.name)).length
})

watch(
  () => props.show,
  (open) => {
    if (open) void load()
  },
)

// Stale-request token for load(): closing the dialog mid-fetch and reopening
// it overlaps two loads, and without the guard the OLDER response could land
// last — replacing the fresh rows, or clearing the spinner while the current
// request is still running. Same pattern as the polling generation below.
let loadGeneration = 0

async function load() {
  const generation = ++loadGeneration
  loading.value = true
  loadFailure.value = NO_CATALOGUE_FAILURE
  rows.value = []
  checkedKeys.value = []
  try {
    const [catalog, existing] = await Promise.all([
      listModelsForProvider(props.providerId),
      listProviderCandidates(props.providerId),
    ])
    if (generation !== loadGeneration) return
    const failure = catalogueFailure(t, catalog)
    if (failure.kind !== 'none') {
      loadFailure.value = failure
      return
    }
    // Ids that could never be imported (blank, or over the public model-name
    // length cap — see MAX_IMPORTABLE_NAME_LENGTH) are normalized and
    // filtered before ANY request: one garbage catalog entry would otherwise
    // 400 the whole suggest-prices call and kill the dialog for every valid
    // id.
    const names = normalizeCatalogNames(catalog.models)
    // The suggest-prices endpoint caps one request, so a catalog larger than
    // the cap goes up as several sequential requests whose results merge —
    // rows past the cap would otherwise silently lose their suggestions and
    // stay unchecked, breaking the smart default exactly on large catalogs.
    const prices = {} as Awaited<ReturnType<typeof suggestPrices>>['prices']
    for (const batch of chunkByCap(names, IMPORT_BATCH_CAP)) {
      const res = await suggestPrices(props.providerId, batch)
      if (generation !== loadGeneration) return
      Object.assign(prices, res.prices)
    }
    // "Already added" mirrors the server's skip rule: the provider maps a
    // model of that NAME (external name = upstream name on import). The full
    // candidate rows go in so mappings still awaiting a probe verdict come
    // back selectable — re-importing them requeues their lost probes.
    rows.value = buildImportRows(names, existing.list, prices)
    checkedKeys.value = rows.value.filter((r) => r.checked).map((r) => r.name)
  } catch (err) {
    // Before the stale guard on purpose: a superseded fetch that hit session
    // expiry is still a session expiry.
    if (redirectIfSessionExpired(err, router)) return
    if (generation !== loadGeneration) return
    // The request itself failed, so there is no response to classify.
    loadFailure.value = catalogueFailure(t, null)
    message.error(displayMessage(err, t))
  } finally {
    if (generation === loadGeneration) loading.value = false
  }
}

function onCheckedKeys(keys: Array<string | number>) {
  checkedKeys.value = keys
}

function reset() {
  stopPolling()
  // A catalog load still in flight when the dialog closed must not write rows
  // or raise an error toast into a dialog that no longer exists (or into the
  // fresh load of a reopened one).
  loadGeneration++
  rows.value = []
  checkedKeys.value = []
  loadFailure.value = NO_CATALOGUE_FAILURE
  phase.value = 'select'
  importedIds.value = []
  progressRows.value = []
  progress.value = { total: 0, passed: 0, failed: 0, inconclusive: 0, pending: 0, done: true }
}

function stopPolling() {
  dialogGeneration++
  progressPoll.stop()
}

function startProgressPoll() {
  progressPoll.start(async (isCurrent) => {
    try {
      const { list } = await listProviderCandidates(props.providerId)
      if (!isCurrent()) return 'stop'
      const { progress: p, rows: r } = summarizeImportProgress(list, importedIds.value)
      progress.value = p
      progressRows.value = r
      return p.done ? 'done' : 'again'
    } catch (err) {
      // A lapsed session must reach the login page, not be retried every tick
      // against a dead session — checked before the currency guard on purpose.
      if (redirectIfSessionExpired(err, router)) return 'stop'
      // Any other failed poll is retried with backoff; the probes keep
      // running server-side regardless, so there is nothing to surface per
      // attempt.
      return 'error'
    }
  })
}

function suggestionTag(row: ImportRow) {
  if (row.added && row.unfinished)
    return h(NTag, { size: 'small', bordered: false, type: 'warning' }, { default: () => t('models.importUnfinished') })
  if (row.added) return h(NTag, { size: 'small', bordered: false }, { default: () => t('models.importAdded') })
  if (row.priceSource === 'history')
    return h(NTag, { size: 'small', bordered: false, type: 'success' }, { default: () => t('models.importPriceHistory') })
  if (row.priceSource === 'seed')
    return h(NTag, { size: 'small', bordered: false, type: 'success' }, { default: () => t('models.importPriceHit') })
  return h(NTag, { size: 'small', bordered: false, type: 'warning' }, { default: () => t('models.importNoPrice') })
}

function priceInput(row: ImportRow, field: 'inputPrice' | 'outputPrice' | 'cacheWritePrice' | 'cacheReadPrice') {
  if (row.added) return h('span', { class: 'candidate-muted' }, '-')
  return h(NInputNumber, {
    value: row[field],
    size: 'small',
    min: 0,
    showButton: false,
    placeholder: '0',
    'onUpdate:value': (v: number | null) => {
      row[field] = v
    },
  })
}

const columns = computed<DataTableColumns<ImportRow>>(() => [
  {
    type: 'selection',
    disabled: (row: ImportRow) => row.added && !row.unfinished,
  },
  {
    title: columnTitle(t('models.importModelName'), t('models.importModelName_tip')),
    key: 'name',
    minWidth: 220,
    render: (row) => h('span', { class: row.added ? 'candidate-muted' : undefined }, row.name),
  },
  {
    title: columnTitle(t('models.importPriceSuggestion'), t('models.importPriceSuggestion_tip')),
    key: 'suggestion',
    width: 110,
    render: suggestionTag,
  },
  {
    title: columnTitle(t('models.importInputPrice'), t('models.inputPrice_tip')),
    key: 'inputPrice',
    width: 110,
    render: (row) => priceInput(row, 'inputPrice'),
  },
  {
    title: columnTitle(t('models.importOutputPrice'), t('models.outputPrice_tip')),
    key: 'outputPrice',
    width: 110,
    render: (row) => priceInput(row, 'outputPrice'),
  },
  {
    title: columnTitle(t('models.importCacheWritePrice'), t('models.cacheWritePrice_tip')),
    key: 'cacheWritePrice',
    width: 110,
    render: (row) => priceInput(row, 'cacheWritePrice'),
  },
  {
    title: columnTitle(t('models.importCacheReadPrice'), t('models.cacheReadPrice_tip')),
    key: 'cacheReadPrice',
    width: 110,
    render: (row) => priceInput(row, 'cacheReadPrice'),
  },
])

function onConfirm() {
  if (phase.value === 'select') {
    void onImport()
    return
  }
  // Progress view's single button: close. The probes keep running server-side;
  // the provider detail page shows their eventual verdicts.
  showModel.value = false
}

async function onImport() {
  const checked = new Set(checkedKeys.value)
  const items = toImportItems(rows.value.map((r) => ({ ...r, checked: checked.has(r.name) })))
  if (items.length === 0) {
    message.warning(t('models.importNothingSelected'))
    return
  }
  importing.value = true
  // Dismissal is blocked while the request runs, but a route change can still
  // unmount and reset the dialog with the request in flight. The generation
  // check keeps such a late success from flipping a dead dialog into the
  // progress phase and starting an invisible poll loop; the parent still gets
  // the 'imported' event either way, because the rows WERE created and it owns
  // the follow-up refresh.
  const generation = dialogGeneration
  // One request carries at most IMPORT_BATCH_CAP items, so a select-all on a
  // larger catalog goes up as sequential compliant batches; the merged result
  // drives the summary and the progress view exactly like a single call. A
  // batch that fails mid-way leaves the EARLIER batches committed and probing
  // — that partial result is still reported and watched, because hiding rows
  // that are genuinely importing would freeze the view on stale state.
  const merged: ImportProviderModelsResult = { items: [], created: 0, appended: 0, skipped: 0 }
  let batchError: unknown = null
  try {
    for (const batch of chunkByCap(items, IMPORT_BATCH_CAP)) {
      const result = await importProviderModels(props.providerId, batch)
      merged.items.push(...result.items)
      merged.created += result.created
      merged.appended += result.appended
      merged.skipped += result.skipped
    }
  } catch (err) {
    if (redirectIfSessionExpired(err, router)) {
      importing.value = false
      return
    }
    batchError = err
  }
  importing.value = false
  if (batchError !== null) {
    message.error(displayMessage(batchError, t))
    // A failed batch may have COMMITTED server-side with only its response
    // lost: its rows are queued and probing, and leaving them out of the
    // watch set would freeze the view on stale state (and tempt a retry that
    // re-arms rows mid-probe). Reconcile against the server: adopt every row
    // from this submission's name set that is visibly owed work. Best-effort
    // — if this read fails too, the detail page's own adoption remains.
    try {
      const { list } = await listProviderCandidates(props.providerId)
      const submitted = new Set(items.map((i) => i.provider_model_name))
      const known = new Set(merged.items.flatMap((it) => (it.candidate_id ? [it.candidate_id] : [])))
      for (const row of list) {
        // Matched by the PUBLIC model name: the submission carries catalog
        // ids, which the server resolves to models by name — and a requeued
        // unfinished mapping keeps whatever upstream name it was created
        // with, which need not equal the public one.
        if (!submitted.has(row.model_name) || known.has(row.candidate_id)) continue
        if (candidateIsOwedWork(row)) {
          merged.items.push({ name: row.model_name, status: 'appended', candidate_id: row.candidate_id, model_id: row.model_id })
        }
      }
    } catch {
      // The error toast above already covers this attempt.
    }
  } else {
    message.success(t('models.importSummary', { created: merged.created, appended: merged.appended, skipped: merged.skipped }))
  }
  if (merged.items.length === 0) return
  emit('imported', merged)
  if (generation !== dialogGeneration) return
  const ids = merged.items.flatMap((item) => (item.candidate_id ? [item.candidate_id] : []))
  if (ids.length === 0) {
    // Everything was skipped — there is nothing to watch being probed.
    if (batchError === null) showModel.value = false
    return
  }
  importedIds.value = ids
  progress.value = { total: ids.length, passed: 0, failed: 0, inconclusive: 0, pending: ids.length, done: false }
  phase.value = 'progress'
  startProgressPoll()
}

const progressColumns = computed<DataTableColumns<ProviderCandidate>>(() => [
  {
    title: columnTitle(t('models.importModelName'), t('models.importModelName_tip')),
    key: 'model_name',
    minWidth: 240,
  },
  {
    title: columnTitle(t('providers.candidateProbeStatus'), t('providers.candidateProbeStatus_tip')),
    key: 'state',
    width: STATUS_COL_WIDTH,
    render: (row) => renderProbeStateTag(t, row, { labelKey: 'models.importStatePending', type: 'info' }),
  },
  {
    title: columnTitle(t('providers.candidateFailReason'), t('providers.candidateFailReason_tip')),
    key: 'last_test_error',
    minWidth: 240,
    render: renderFailReasonCell,
  },
])
</script>

<style scoped>
.import-hint {
  margin: 0 0 var(--space-3);
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
}

.import-progress-bar {
  margin-bottom: var(--space-3);
}

.import-hint--footer {
  margin: var(--space-3) 0 0;
}
</style>
