<!-- frontend/src/components/models/ImportModelsModal.vue -->
<!-- Bulk import as a two-pane master-detail dialog:
     left — the upstream catalogue as selectable rows (checkbox, display name
     in mono, output-modality chips, configured/unconfigured status dot,
     price-suggestion badge) under a toolbar (select-all, name filter,
     selected count);
     right — the detail form bound to the selected row (public name,
     capability declarations, price group). Detail edits write straight onto
     the row object, so the list mirrors them live.

     The progress phase (the post-submit probing watch) is unchanged, and so
     is the wire: batched submission by IMPORT_BATCH_CAP, omit-shaped
     declarations, suggestion prefill and the smart default check. -->
<template>
  <!-- Wider than the 520px form dialogs on purpose: two panes side by side
       (master list + detail form); below the mobile breakpoint the drawer is
       full-width anyway and the panes stack vertically. -->
  <ModalDrawer
    v-model:show="showModel"
    :title="phase === 'select' ? t('models.importTitle') : t('models.importProgressTitle')"
    max-width="980px"
    :mask-closable="false"
    :close-on-esc="false"
    :confirm-text="phase === 'select' ? t('models.importSelected', { count: selectedCount }) : t('models.importClose')"
    :cancel-text="t('models.cancel')"
    :confirm-disabled="phase === 'select' && selectedCount === 0"
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
        <n-spin :show="loading">
          <div class="import-layout" :class="{ 'import-layout--stacked': isMobile }">
            <!-- Master list: selection + identity + a read-only glance at
                 what the detail pane edits. No price columns — the four token
                 slots and the audio slot live in the detail form only. -->
            <section class="import-pane import-pane--list">
              <div class="import-toolbar">
                <n-checkbox
                  class="import-toolbar__all"
                  :checked="allVisibleChecked"
                  :indeterminate="someVisibleChecked && !allVisibleChecked"
                  :disabled="selectableVisible.length === 0"
                  :title="t('models.importSelectAll')"
                  @update:checked="onToggleAll"
                />
                <n-input
                  v-model:value="filter"
                  class="import-toolbar__filter"
                  size="small"
                  clearable
                  :placeholder="t('models.importFilterPlaceholder')"
                />
                <span class="import-toolbar__count">{{ t('models.importSelectedCount', { count: selectedCount }) }}</span>
              </div>
              <!-- Plain list semantics, not listbox/option: each row carries a
                   real checkbox whose checked state IS the selection, and an
                   aria-selected row would additionally announce the detail
                   BINDING as a screen-reader selection — two conflicting
                   selection stories in one row. The detail-bound row is
                   marked with aria-current instead; selection stays solely
                   the checkboxes'.

                   The role="list" element is an inner layer, separate from
                   the bordered scroll box: the filter's empty note must be
                   the list's SIBLING, never a child — a non-listitem owned
                   element inside role="list" is exactly the malformed
                   content browsers repair by stripping the list role. The
                   note stays inside the same visual box, so the empty state
                   looks unchanged. -->
              <div class="import-list">
                <div class="import-list__rows" role="list" :aria-label="t('models.importTitle')">
                  <div
                    v-for="row in visibleRows"
                    :key="row.name"
                    class="import-row"
                    :class="{ 'import-row--selected': row.name === selectedKey }"
                    role="listitem"
                    :aria-current="row.name === selectedKey ? 'true' : undefined"
                    tabindex="0"
                    @click="selectRow(row)"
                    @keydown.enter.prevent="selectRow(row)"
                  >
                    <span class="import-row__check" @click.stop>
                      <n-checkbox
                        class="import-row__check-box"
                        :checked="checkedSet.has(row.name)"
                        :disabled="!isSelectable(row)"
                        @update:checked="(checked: boolean) => onToggleRow(row, checked)"
                      />
                    </span>
                    <!-- The public name when one is declared; the bare
                         upstream id otherwise, muted so the fallback is
                         legible as a fallback. -->
                    <span class="import-row__name" :class="{ 'import-row__name--fallback': row.pubName === '' }">
                      {{ displayOf(row) }}
                    </span>
                    <span class="import-row__mods">
                      <span v-for="axis in row.out" :key="axis" class="import-row__chip">{{ modalityLabel(axis) }}</span>
                    </span>
                    <span
                      class="import-row__dot"
                      :class="`import-row__dot--${statusOf(row)}`"
                      :title="statusTitle(row)"
                    />
                    <span class="import-row__suggestion">
                      <n-tag v-if="row.added && row.unfinished" size="small" :bordered="false" type="warning">
                        {{ t('models.importUnfinished') }}
                      </n-tag>
                      <n-tag v-else-if="row.added" size="small" :bordered="false">{{ t('models.importAdded') }}</n-tag>
                      <span
                        v-else-if="rowBillsPerImage(row)"
                        class="candidate-muted"
                        :title="t('models.importImagePricingHint_tip')"
                      >
                        {{ t('models.importImagePricingHint') }}
                      </span>
                      <n-tag v-else-if="row.priceSource === 'history'" size="small" :bordered="false" type="success">
                        {{ t('models.importPriceHistory') }}
                      </n-tag>
                      <n-tag v-else-if="row.priceSource === 'seed'" size="small" :bordered="false" type="success">
                        {{ t('models.importPriceHit') }}
                      </n-tag>
                      <n-tag v-else size="small" :bordered="false" type="warning">{{ t('models.importNoPrice') }}</n-tag>
                    </span>
                  </div>
                </div>
                <p v-if="rows.length > 0 && visibleRows.length === 0" class="import-list__empty">{{ t('models.importFilterEmpty') }}</p>
              </div>
            </section>
            <!-- Detail: the selected row's form. Every control binds the row
                 object itself, so a rename re-renders the list row and a new
                 capability declaration flips the status dot on the spot. -->
            <section ref="detailEl" class="import-pane import-pane--detail">
              <template v-if="selected">
                <header class="import-detail__header">
                  <span class="import-row__dot" :class="`import-row__dot--${statusOf(selected)}`" :title="statusTitle(selected)" />
                  <span class="import-detail__name" :class="{ 'import-row__name--fallback': selected.pubName === '' }">
                    {{ displayOf(selected) }}
                  </span>
                  <span class="import-detail__upstream">{{ selected.name }}</span>
                </header>
                <!-- An added row submits names only (see toImportItems): its
                     declarations are server-owned, so the whole form reads
                     disabled instead of pretending to collect anything. -->
                <p v-if="selected.added" class="import-detail__note">{{ t('models.importDetailAddedNote') }}</p>
                <div class="import-detail__form">
                  <div class="import-field">
                    <span class="import-field__label">{{ t('models.importDetailPubName') }}</span>
                    <n-input
                      v-model:value="selected.pubName"
                      class="import-detail__pubname"
                      :disabled="selected.added"
                      :placeholder="t('models.importDetailPubNamePlaceholder')"
                    />
                  </div>
                  <div class="import-field">
                    <span class="import-field__label">{{ t('models.contextLength') }}</span>
                    <n-input-number
                      v-model:value="selected.ctxLength"
                      class="import-detail__ctx"
                      :min="1"
                      :precision="0"
                      :disabled="selected.added"
                      :placeholder="t('models.undeclaredPlaceholder')"
                    />
                    <div class="import-detail__presets">
                      <n-button
                        v-for="preset in CONTEXT_LENGTH_PRESETS"
                        :key="preset.value"
                        size="tiny"
                        quaternary
                        type="primary"
                        :disabled="selected.added"
                        @click="selected.ctxLength = preset.value"
                      >
                        {{ preset.label }}
                      </n-button>
                    </div>
                  </div>
                  <div class="import-field">
                    <span class="import-field__label">{{ t('models.inputModalities') }}</span>
                    <!-- Empty selection = undeclared, the input axis's
                         meaningful no-declaration state. -->
                    <n-select
                      v-model:value="selectedInMods"
                      class="import-detail__in-mods"
                      multiple
                      clearable
                      :options="inputModalityOptions"
                      :disabled="selected.added"
                      :placeholder="t('models.undeclaredPlaceholder')"
                    />
                  </div>
                  <div class="import-field">
                    <span class="import-field__label">{{ t('models.outputModalities') }}</span>
                    <!-- Video stays absent: an import cannot fill the
                         per-second price table a video row needs (same
                         reason the old single-select column lacked it).
                         Audio is exclusive — the shared watch below
                         collapses any selection it joins. -->
                    <n-select
                      v-model:value="selectedOut"
                      class="import-detail__out-mods"
                      multiple
                      :options="outputModalityChoices"
                      :disabled="selected.added"
                    />
                  </div>
                  <div class="import-field">
                    <span class="import-field__label">{{ t('models.supportsTools') }}</span>
                    <n-select
                      v-model:value="selected.tools"
                      class="import-detail__tools"
                      :options="triStateOptions"
                      :disabled="selected.added"
                    />
                  </div>
                  <div class="import-field">
                    <span class="import-field__label">{{ t('models.supportsReasoning') }}</span>
                    <n-select
                      v-model:value="selected.reasoning"
                      class="import-detail__reasoning"
                      :options="triStateOptions"
                      :disabled="selected.added"
                    />
                  </div>
                  <div class="import-field">
                    <span class="import-field__label">{{ t('models.temperature') }}</span>
                    <!-- Same [0,2] closed range the model edit dialog binds;
                         cleared input = no override. -->
                    <n-input-number
                      v-model:value="selected.temp"
                      class="import-detail__temp"
                      :min="0"
                      :max="2"
                      :step="0.1"
                      :disabled="selected.added"
                      :placeholder="t('models.undeclaredPlaceholder')"
                    />
                  </div>
                  <div class="import-field">
                    <span class="import-field__label">{{ t('models.maxOutput') }}</span>
                    <n-input-number
                      v-model:value="selected.maxOut"
                      class="import-detail__maxout"
                      :min="0"
                      :precision="0"
                      :disabled="selected.added"
                      :placeholder="t('models.undeclaredPlaceholder')"
                    />
                  </div>
                </div>
                <div class="import-detail__prices">
                  <span class="import-field__label">{{ t('models.importDetailPrices') }}</span>
                  <div class="import-detail__price-grid">
                    <div class="import-field">
                      <span class="import-field__sublabel">{{ t('models.importInputPrice') }}</span>
                      <n-input-number
                        v-model:value="selected.inputPrice"
                        class="import-detail__input-price"
                        :min="0"
                        :show-button="false"
                        :disabled="tokenPriceDisabled(selected)"
                        placeholder="0"
                      />
                    </div>
                    <div class="import-field">
                      <span class="import-field__sublabel">{{ t('models.importOutputPrice') }}</span>
                      <n-input-number
                        v-model:value="selected.outputPrice"
                        class="import-detail__output-price"
                        :min="0"
                        :show-button="false"
                        :disabled="tokenPriceDisabled(selected)"
                        placeholder="0"
                      />
                    </div>
                    <div class="import-field">
                      <span class="import-field__sublabel">{{ t('models.importCacheWritePrice') }}</span>
                      <n-input-number
                        v-model:value="selected.cacheWritePrice"
                        class="import-detail__cache-write-price"
                        :min="0"
                        :show-button="false"
                        :disabled="tokenPriceDisabled(selected)"
                        placeholder="0"
                      />
                    </div>
                    <div class="import-field">
                      <span class="import-field__sublabel">{{ t('models.importCacheReadPrice') }}</span>
                      <n-input-number
                        v-model:value="selected.cacheReadPrice"
                        class="import-detail__cache-read-price"
                        :min="0"
                        :show-button="false"
                        :disabled="tokenPriceDisabled(selected)"
                        placeholder="0"
                      />
                    </div>
                    <div class="import-field">
                      <span class="import-field__sublabel">{{ t('models.audioUnitPrice') }}</span>
                      <n-input-number
                        v-model:value="selected.audioUnitPrice"
                        class="import-detail__audio-price"
                        :min="0"
                        :show-button="false"
                        :disabled="selected.added || !isAudioRow(selected)"
                        placeholder="0"
                      />
                    </div>
                  </div>
                  <p class="import-detail__price-hint">{{ t('models.importDetailPricedHint') }}</p>
                </div>
                <div class="import-detail__footer">
                  <!-- Rendered disabled on purpose: per-row probing from this
                       dialog is a later batch; the tooltip says so instead of
                       leaving a dead button unexplained. -->
                  <n-button
                    class="import-detail__probe"
                    size="small"
                    disabled
                    :title="t('models.importDetailProbeDisabledTip')"
                  >
                    {{ t('models.importDetailProbe') }}
                  </n-button>
                  <p class="import-detail__hint">{{ t('models.importDetailUnpricedHint') }}</p>
                </div>
              </template>
            </section>
          </div>
        </n-spin>
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
          :data="progressEntries"
          :row-key="progressRowKey"
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
// NProgress, NSpin and NCheckbox are not in main.ts's create() list — they
// must be imported explicitly or Vue renders <n-progress> & co. as unknown
// elements with zero build errors.
import {
  NButton,
  NCheckbox,
  NInput,
  NInputNumber,
  NProgress,
  NSelect,
  NSpin,
  NTag,
  useMessage,
  type DataTableColumns,
  type SelectOption,
} from 'naive-ui'
import { AlertTriangle, Inbox } from '@lucide/vue'
import { listModelsForProvider } from '../../api/providers'
import { importProviderModels, listProviderCandidates, suggestPrices, type ImportItemResult, type ImportProviderModelsResult, type ProviderCandidate } from '../../api/models'
import { displayMessage } from '../../api/client'
import { buildImportRows, chunkByCap, IMPORT_BATCH_CAP, modalityOf, normalizeCatalogNames, statusOf, toImportItems, type ImportRow } from '../../utils/importRows'
import { catalogueFailure, NO_CATALOGUE_FAILURE, type CatalogueFailure } from '../../utils/catalogueFailure'
import { candidateIsOwedWork, PROGRESS_POLL_BACKOFF_CAP_MS, PROGRESS_POLL_BASE_MS, skipsWorthReading, summarizeImportProgress, type ImportProgress } from '../../utils/importProgress'
import { renderFailReasonCell, renderProbeStateTag } from '../../utils/probeStateTag'
import { outputModalityOptions } from '../../utils/modalityOptions'
import { CONTEXT_LENGTH_PRESETS } from '../../utils/contextLengthPresets'
import ModalDrawer from '../common/ModalDrawer.vue'
import EmptyState from '../EmptyState.vue'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { redirectIfSessionExpired } from '../../utils/sessionExpiredRedirect'
import { useBackoffPoll } from '../../composables/useBackoffPoll'
import { useIsMobile } from '../../composables/useIsMobile'
import { useModalityExclusivity } from '../../composables/useModalityExclusivity'

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

// The detail pane's bound row: catalog order picks the default (first row)
// and a click moves it. Keyed by the upstream id — the row's stable identity,
// untouched by a public-name edit.
const filter = ref('')
const selectedKey = ref<string | null>(null)
const detailEl = ref<HTMLElement | null>(null)
const isMobile = useIsMobile()

// After a successful import the dialog switches to the progress view and
// polls the imported mappings' verification states until every one has a
// verdict — closable at any time, the probing continues server-side.
const phase = ref<'select' | 'progress'>('select')
const importedIds = ref<number[]>([])
const progressRows = ref<ProviderCandidate[]>([])
// Rows the import refused to store, with a reason worth reading. "exists"
// skips are deliberately absent: a settled mapping re-submitted is routine,
// not a problem to solve. Static — unlike the probed rows there is nothing
// to poll.
const skippedItems = ref<ImportItemResult[]>([])
const progress = ref<ImportProgress>({ total: 0, passed: 0, failed: 0, inconclusive: 0, pending: 0, done: true })
// The shared generation-guarded backoff loop; see useBackoffPoll. dialogGeneration
// additionally invalidates the batched-submit flow when the dialog closes —
// the submit runs OUTSIDE the poll loop, so the poll's own guard cannot cover
// it.
const progressPoll = useBackoffPoll(PROGRESS_POLL_BASE_MS, PROGRESS_POLL_BACKOFF_CAP_MS)
let dialogGeneration = 0

// ---- Master list ----

function isSelectable(row: ImportRow): boolean {
  return !row.added || row.unfinished
}

function displayOf(row: ImportRow): string {
  return row.pubName !== '' ? row.pubName : row.name
}

const checkedSet = computed(() => new Set(checkedKeys.value))

const selectedCount = computed(() => rows.value.filter((r) => isSelectable(r) && checkedSet.value.has(r.name)).length)

// The filter reads both names: the public name an admin is shaping (which a
// rename may have just diverged from the upstream id) and the upstream id
// itself (which is what a catalogue copy-paste carries).
const visibleRows = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (q === '') return rows.value
  return rows.value.filter((r) => displayOf(r).toLowerCase().includes(q) || r.name.toLowerCase().includes(q))
})

const selectableVisible = computed(() => visibleRows.value.filter(isSelectable))
const allVisibleChecked = computed(
  () => selectableVisible.value.length > 0 && selectableVisible.value.every((r) => checkedSet.value.has(r.name)),
)
const someVisibleChecked = computed(() => selectableVisible.value.some((r) => checkedSet.value.has(r.name)))

// Select-all scopes to the rows the filter currently shows, so it means the
// same thing before and after a narrowing search.
function onToggleAll(checked: boolean) {
  const set = new Set(checkedKeys.value)
  for (const row of selectableVisible.value) {
    if (checked) set.add(row.name)
    else set.delete(row.name)
  }
  checkedKeys.value = [...set]
}

function onToggleRow(row: ImportRow, checked: boolean) {
  const set = new Set(checkedKeys.value)
  if (checked) set.add(row.name)
  else set.delete(row.name)
  checkedKeys.value = [...set]
}

function statusTitle(row: ImportRow): string {
  return statusOf(row) === 'configured' ? t('models.importRowConfigured') : t('models.importRowUnconfigured')
}

function modalityLabel(axis: string): string {
  if (axis === 'image') return t('models.outputModalityImage')
  if (axis === 'audio') return t('models.outputModalityAudio')
  return t('models.outputModalityText')
}

const selected = computed(() => rows.value.find((r) => r.name === selectedKey.value) ?? null)

function selectRow(row: ImportRow) {
  selectedKey.value = row.name
  // Stacked layout: the detail sits below the list, so a tap must bring it
  // into view — the desktop grid has both panes side by side and needs none.
  if (isMobile.value) detailEl.value?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

// ---- Detail form ----

// The output-axes picker binds the selected row's declaration. The shared
// useModalityExclusivity watch runs on this two-way shim, so the exclusive
// ids (audio here; video is not offered) collapse the selection exactly as
// they do on the model dialogs' own form objects.
const selectedOut = computed<string[]>({
  get: () => selected.value?.out ?? [],
  set: (v) => {
    if (selected.value) selected.value.out = v
  },
})
useModalityExclusivity({
  get outputModalities() {
    return selectedOut.value
  },
  set outputModalities(v: string[]) {
    selectedOut.value = v
  },
})

// The input axes keep the tri-state-by-absence shape: cleared = null =
// undeclared, never an empty "text only" claim.
const selectedInMods = computed<string[]>({
  get: () => selected.value?.inMods ?? [],
  set: (v) => {
    if (selected.value) selected.value.inMods = v.length > 0 ? [...v] : null
  },
})

const inputModalityOptions = computed<SelectOption[]>(() => [
  { label: t('models.inputModalityText'), value: 'text' },
  { label: t('models.inputModalityImage'), value: 'image' },
  { label: t('models.inputModalityAudio'), value: 'audio' },
  { label: t('models.inputModalityVideo'), value: 'video' },
])

const outputModalityChoices = computed<SelectOption[]>(() =>
  outputModalityOptions(t).filter((option) => option.value !== 'video'),
)

// The tool/reasoning tri-state, on the row's own 0/1/2 vocabulary.
const triStateOptions = computed<SelectOption[]>(() => [
  { label: t('models.triStateUnknown'), value: 0 },
  { label: t('models.triStateYes'), value: 1 },
  { label: t('models.triStateNo'), value: 2 },
])

// A row the admin marked as image-ONLY settles per delivered image, not per
// token — the four per-M price cells are inert for it, and saying so beats
// showing editable fields that do nothing. A 'both' row is NOT included: the
// mapping still serves chat traffic and bills tokens. An audio row bills per
// character: its single price replaces the token slots.
function rowBillsPerImage(row: ImportRow): boolean {
  return !row.added && modalityOf(row.out) === 'image'
}

function isAudioRow(row: ImportRow): boolean {
  return modalityOf(row.out) === 'audio'
}

function tokenPriceDisabled(row: ImportRow): boolean {
  return row.added || rowBillsPerImage(row) || isAudioRow(row)
}

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
  selectedKey.value = null
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
    // The detail pane is persistent — bind it to the catalog's first row so
    // the form is never empty while the list has anything to show.
    selectedKey.value = rows.value[0]?.name ?? null
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

function reset() {
  stopPolling()
  // A catalog load still in flight when the dialog closed must not write rows
  // or raise an error toast into a dialog that no longer exists (or into the
  // fresh load of a reopened one).
  loadGeneration++
  rows.value = []
  checkedKeys.value = []
  filter.value = ''
  selectedKey.value = null
  loadFailure.value = NO_CATALOGUE_FAILURE
  phase.value = 'select'
  importedIds.value = []
  progressRows.value = []
  skippedItems.value = []
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
  skippedItems.value = []
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
        // Matched by the UPSTREAM id (provider_model_name): that is the key
        // the submission carries and the key the server stores the mapping
        // under — a row re-imported under a custom public name must still be
        // adopted, which a model_name match would miss.
        if (!submitted.has(row.provider_model_name) || known.has(row.candidate_id)) continue
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
  skippedItems.value = skipsWorthReading(merged.items)
  const ids = merged.items.flatMap((item) => (item.candidate_id ? [item.candidate_id] : []))
  if (ids.length === 0) {
    // Nothing to watch being probed. Rows the import refused still deserve
    // their explanation, so the progress view opens for those alone.
    if (batchError === null && skippedItems.value.length === 0) showModel.value = false
    if (skippedItems.value.length > 0) phase.value = 'progress'
    return
  }
  importedIds.value = ids
  progress.value = { total: ids.length, passed: 0, failed: 0, inconclusive: 0, pending: ids.length, done: false }
  phase.value = 'progress'
  startProgressPoll()
}

// One row of the progress view: a mapping being probed, or a row the import
// refused to store. The refused shape is display-only — there is no candidate
// behind it to poll.
type ProgressEntry = ProviderCandidate | { skipped: true; name: string; reason: string }

function isSkippedEntry(row: ProgressEntry): row is { skipped: true; name: string; reason: string } {
  return 'skipped' in row
}

const progressEntries = computed<ProgressEntry[]>(() => [
  ...progressRows.value,
  ...skippedItems.value.map((it) => ({ skipped: true as const, name: it.name, reason: it.reason ?? '' })),
])

function progressRowKey(row: ProgressEntry): string | number {
  return isSkippedEntry(row) ? `skipped:${row.name}` : row.candidate_id
}

function skipReasonText(reason: string): string {
  if (reason === 'modality_mismatch') return t('models.importSkipModalityMismatch')
  if (reason === 'capability_mismatch') return t('models.importSkipCapabilityMismatch')
  if (reason === 'invalid') return t('models.importSkipInvalid')
  return reason
}

const progressColumns = computed<DataTableColumns<ProgressEntry>>(() => [
  {
    title: columnTitle(t('models.importModelName'), t('models.importModelName_tip')),
    key: 'model_name',
    minWidth: 240,
    render: (row) => h('span', undefined, isSkippedEntry(row) ? row.name : row.model_name),
  },
  {
    title: columnTitle(t('providers.candidateProbeStatus'), t('providers.candidateProbeStatus_tip')),
    key: 'state',
    width: STATUS_COL_WIDTH,
    render: (row) =>
      isSkippedEntry(row)
        ? h(NTag, { size: 'small', bordered: false, type: 'warning' }, { default: () => t('models.importSkippedTag') })
        : renderProbeStateTag(t, row, { labelKey: 'models.importStatePending', type: 'info' }),
  },
  {
    title: columnTitle(t('providers.candidateFailReason'), t('providers.candidateFailReason_tip')),
    key: 'last_test_error',
    minWidth: 240,
    render: (row) => (isSkippedEntry(row) ? skipReasonText(row.reason) : renderFailReasonCell(row)),
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

.import-layout {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: var(--space-4);
  align-items: start;
}

.import-layout--stacked {
  grid-template-columns: minmax(0, 1fr);
}

.import-toolbar {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin-bottom: var(--space-2);
}

.import-toolbar__filter {
  flex: 1;
}

.import-toolbar__count {
  flex-shrink: 0;
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
  white-space: nowrap;
}

/* Visual box only — role="list" lives on the inner .import-list__rows so the
   filter's empty note can sit beside the list instead of inside it. */
.import-list {
  max-height: 420px;
  overflow-y: auto;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  background: var(--color-surface);
}

.import-layout--stacked .import-list {
  max-height: 40vh;
}

.import-row {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto auto auto;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2);
  border-bottom: 1px solid var(--color-border-subtle);
  cursor: pointer;
}

.import-row:last-child {
  border-bottom: none;
}

.import-row:hover,
.import-row:focus-visible {
  background: var(--color-surface-hover);
}

.import-row--selected {
  background: var(--color-accent-subtle);
}

.import-row__check {
  display: inline-flex;
}

.import-row__name {
  font-family: var(--font-mono);
  font-size: var(--text-sm);
  overflow-wrap: anywhere;
}

.import-row__name--fallback {
  color: var(--color-text-muted);
}

.import-row__mods {
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
  justify-content: flex-end;
}

.import-row__chip {
  font-size: var(--text-xs);
  line-height: 1;
  padding: 3px 6px;
  border-radius: var(--radius-full);
  background: var(--color-bg-soft);
  border: 1px solid var(--color-border);
  color: var(--color-text-secondary);
  white-space: nowrap;
}

.import-row__dot {
  flex-shrink: 0;
  width: 8px;
  height: 8px;
  border-radius: var(--radius-full);
}

.import-row__dot--unconfigured {
  background: var(--color-text-muted);
}

.import-row__dot--configured {
  background: var(--color-success);
}

.import-row__suggestion {
  display: inline-flex;
  min-width: 0;
}

.import-list__empty {
  margin: 0;
  padding: var(--space-3);
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
  text-align: center;
}

.import-pane--detail {
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
  padding: var(--space-3);
  background: var(--color-surface);
}

.import-detail__header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin-bottom: var(--space-3);
}

.import-detail__name {
  font-family: var(--font-mono);
  font-weight: 600;
  overflow-wrap: anywhere;
}

.import-detail__upstream {
  color: var(--color-text-muted);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  overflow-wrap: anywhere;
}

.import-detail__note {
  margin: 0 0 var(--space-3);
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-md);
  background: var(--color-warning-subtle);
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
}

.import-detail__form {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--space-3);
}

.import-layout--stacked .import-detail__form {
  grid-template-columns: minmax(0, 1fr);
}

.import-field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}

.import-field__label {
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
}

.import-field__sublabel {
  color: var(--color-text-muted);
  font-size: var(--text-xs);
}

.import-detail__presets {
  display: flex;
  gap: 4px;
  flex-wrap: wrap;
}

.import-detail__prices {
  margin-top: var(--space-3);
  padding-top: var(--space-3);
  border-top: 1px solid var(--color-border-subtle);
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.import-detail__price-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(130px, 1fr));
  gap: var(--space-2);
}

.import-detail__price-hint {
  margin: 0;
  color: var(--color-text-secondary);
  font-size: var(--text-xs);
}

.import-detail__footer {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-top: var(--space-3);
  padding-top: var(--space-3);
  border-top: 1px solid var(--color-border-subtle);
}

.import-detail__footer .import-detail__probe {
  flex-shrink: 0;
}

.import-detail__hint {
  margin: 0;
  color: var(--color-text-secondary);
  font-size: var(--text-xs);
}
</style>
