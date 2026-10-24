<!-- frontend/src/components/models/ImportModelsModal.vue -->
<!-- Bulk import as a two-pane master-detail dialog:
     left — the upstream catalogue as selectable rows (checkbox, display name
     in mono, output-modality chips, configured/unconfigured status dot,
     price-suggestion badge) under a toolbar (select-all, name filter,
     selected count);
     right — the detail form bound to the selected row (public name,
     capability declarations, price group). Detail edits write straight onto
     the row object, so the list mirrors them live. The footer's Probe button
     fires the standalone pre-import probe for the selected row and renders
     the verdict bar; a confirmed capability fills the row's UNDECLARED slot
     (badged "probe suggested"), a declared one is never overwritten — the
     kept-hint says so instead.

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
                    <span class="import-field__label">
                      {{ t('models.inputModalities') }}
                      <!-- The probe's vision verdict filled this slot (it was
                           undeclared when the probe confirmed vision input);
                           the badge says where the value came from. -->
                      <n-tag
                        v-if="selectedProbeState?.visionSuggestion === 'applied'"
                        class="import-field__suggested"
                        size="small"
                        :bordered="false"
                        type="info"
                      >
                        {{ t('models.importProbeSuggestedTag') }}
                      </n-tag>
                    </span>
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
                    <span class="import-field__label">
                      {{ t('models.supportsTools') }}
                      <!-- Same probe-suggestion badge as the input axes: the
                           slot was undeclared and the function-calling probe
                           confirmed the capability. -->
                      <n-tag
                        v-if="selectedProbeState?.toolsSuggestion === 'applied'"
                        class="import-field__suggested"
                        size="small"
                        :bordered="false"
                        type="info"
                      >
                        {{ t('models.importProbeSuggestedTag') }}
                      </n-tag>
                    </span>
                    <n-select
                      v-model:value="selectedTools"
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
                <!-- The last probe's conclusion bar for the selected row:
                     one ✓✗/?/— verdict per probe (basic text, streaming,
                     function calling, vision input) plus the total time in
                     flight. basic/streaming have no form counterpart — they
                     report here only. -->
                <div v-if="selectedProbeState" class="import-detail__probe-report" role="status">
                  <span class="import-detail__probe-report__title">{{ t('models.importProbeReportTitle') }}</span>
                  <span v-for="entry in probeBarEntries" :key="entry.label" class="import-detail__probe-report__entry">
                    {{ entry.label }}
                    <b :class="`import-detail__probe-report__icon--${entry.tone}`" :title="entry.note">{{ entry.icon }}</b>
                    <span v-if="entry.note" class="import-detail__probe-report__note">{{ entry.note }}</span>
                  </span>
                  <span class="import-detail__probe-report__duration">{{ t('models.importProbeDuration', { ms: probeTotalMs }) }}</span>
                </div>
                <!-- The failing arms' refusals in the upstream's own words,
                     one pre each: a failed basic probe (whose detail is often
                     the only reason available — the no-usable-key path has
                     no verdict bar note that explains it) and the vision arm
                     whenever it ran and did not confirm vision — "not
                     supported" and "unknown" alike carry a reason worth
                     reading verbatim. -->
                <pre v-for="detail in failureDetails" :key="detail" class="upstream-detail">{{ detail }}</pre>
                <!-- Suggestions the probe made but did NOT apply: the row
                     already declared the capability, so nothing was written —
                     saying so beats a silent no-op. -->
                <p v-if="selectedProbeState?.toolsSuggestion === 'notApplied'" class="import-detail__suggestion">
                  {{ t('models.importProbeToolsKept') }}
                </p>
                <p v-if="selectedProbeState?.visionSuggestion === 'notApplied'" class="import-detail__suggestion">
                  {{ t('models.importProbeVisionKept') }}
                </p>
                <div class="import-detail__footer">
                  <!-- The standalone pre-import probe: enabled for every row
                       this dialog can still shape. An added row's declarations
                       are server-owned, so there is nothing to fill and it
                       stays disabled with its requeue tooltip. While a probe
                       runs the button shows the progress and refuses
                       re-entry. -->
                  <n-button
                    class="import-detail__probe"
                    size="small"
                    :disabled="selected.added || probeRunning"
                    :loading="probeRunning"
                    :title="selected.added ? t('models.importDetailProbeDisabledTip') : t('models.importDetailProbeTip')"
                    @click="onProbe"
                  >
                    {{ probeRunning ? t('models.importProbeRunning') : t('models.importDetailProbe') }}
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
import {
  importProviderModels,
  listProviderCandidates,
  probeProviderModel,
  suggestPrices,
  type ImportItemResult,
  type ImportProviderModelsResult,
  type ProbeReport,
  type ProviderCandidate,
  type ProviderModelProbeResult,
} from '../../api/models'
import { displayMessage } from '../../api/client'
import { buildImportRows, chunkByCap, IMPORT_BATCH_CAP, modalityOf, normalizeCatalogNames, statusOf, toImportItems, type ImportRow } from '../../utils/importRows'
import { catalogueFailure, NO_CATALOGUE_FAILURE, type CatalogueFailure } from '../../utils/catalogueFailure'
import { candidateIsOwedWork, PROGRESS_POLL_BACKOFF_CAP_MS, PROGRESS_POLL_BASE_MS, skipsWorthReading, summarizeImportProgress, type ImportProgress } from '../../utils/importProgress'
import { renderFailReasonCell, renderProbeStateTag } from '../../utils/probeStateTag'
import { testOutcomeLabel } from '../../utils/testOutcomeDisplay'
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
// undeclared, never an empty "text only" claim. Edits go through the shim so
// a hand change can retire a probe suggestion it made stale (see
// syncSuggestionStateAfterEdit).
const selectedInMods = computed<string[]>({
  get: () => selected.value?.inMods ?? [],
  set: (v) => {
    if (!selected.value) return
    selected.value.inMods = v.length > 0 ? [...v] : null
    syncSuggestionStateAfterEdit(selected.value)
  },
})

// The tools tri-state binds through a shim for the same reason: the setter is
// the one hook every manual edit passes through.
const selectedTools = computed<0 | 1 | 2>({
  get: () => selected.value?.tools ?? 0,
  set: (v) => {
    if (!selected.value) return
    selected.value.tools = v
    syncSuggestionStateAfterEdit(selected.value)
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

// ---- Per-row probe (try before you import) ----

// One probe's consequences for its row: the report to render, plus which of
// the suggestions the backfill actually applied. Keyed by the upstream id —
// the row's stable identity — so each row keeps its own last report and the
// suggestion badges name where a filled slot came from.
interface RowProbeState {
  result: ProviderModelProbeResult
  // 'applied' = the row was undeclared and the confirmed capability was
  // written into the form (badged on the field label) — or the value still is
  // the previous probe's untouched backfill, so a re-probe keeps the badge;
  // 'notApplied' = the probe confirmed the capability but the row already
  // declared it, so only the kept-hint shows. '' = the probe did not confirm
  // the capability, so there is no suggestion at all.
  toolsSuggestion: '' | 'applied' | 'notApplied'
  visionSuggestion: '' | 'applied' | 'notApplied'
}
const probeStates = ref<Record<string, RowProbeState>>({})
const probeRunning = ref(false)

// The input-modality set the vision backfill writes when it fills an
// undeclared slot; the staleness check compares the field against it to know
// whether the value the probe wrote is still what the field holds.
const PROBE_SUGGESTED_IN_MODS = ['text', 'image']

const selectedProbeState = computed<RowProbeState | null>(
  () => (selected.value ? probeStates.value[selected.value.name] ?? null : null),
)

async function onProbe() {
  const row = selected.value
  if (!row || row.added || probeRunning.value) return
  probeRunning.value = true
  // Same stale-guard as the submit flow: a dialog closed mid-probe must not
  // have a late report written into the fresh session's rows.
  const generation = dialogGeneration
  try {
    // The row's output declaration rides along: it decides which endpoint
    // family the probe speaks (a declared image model is probed through the
    // images endpoint, not chat).
    const result = await probeProviderModel(props.providerId, {
      provider_model_name: row.name,
      output_modalities: [...row.out],
    })
    if (generation !== dialogGeneration) return
    applyProbeSuggestions(row, result)
  } catch (err) {
    if (redirectIfSessionExpired(err, router)) return
    // A stale probe's failure is dropped silently too: an error toast into
    // the reopened session would report a probe that session never sent.
    if (generation !== dialogGeneration) return
    message.error(displayMessage(err, t))
  } finally {
    // Only the generation that started the probe clears the in-flight flag.
    // reset() has already handed the fresh session a clean flag, and the old
    // promise landing there must not stomp a probe the new session began.
    if (generation === dialogGeneration) probeRunning.value = false
  }
}

// applyProbeSuggestions runs the suggestion backfill for one probe result.
// A confirmed capability fills an UNDECLARED slot only — a slot the admin
// already declared is never overwritten, and an inconclusive (null) or
// negative verdict never writes anything. basic/streaming have no form
// counterpart (the report bar shows them), and no context length is ever
// suggested.
function applyProbeSuggestions(row: ImportRow, result: ProviderModelProbeResult) {
  const state: RowProbeState = { result, toolsSuggestion: '', visionSuggestion: '' }
  // The previous round's state for this row: a re-probe consults it so a slot
  // the last probe filled — and no hand edit has touched since (any hand edit
  // retires the applied state, see syncSuggestionStateAfterEdit) — keeps its
  // applied badge instead of being re-read as an admin declaration.
  const prev = probeStates.value[row.name]
  const fc = result.report.function_calling
  if (fc.ran && fc.supported === true) {
    if (row.tools === 0) {
      row.tools = 1
      state.toolsSuggestion = 'applied'
    } else if (row.tools === 1 && prev?.toolsSuggestion === 'applied') {
      // Same value the probe suggests, and it is still the previous probe's
      // backfill: keep the badge. Recording notApplied here would demote the
      // badge to the kept-hint and attribute the value to a declaration the
      // admin never made.
      state.toolsSuggestion = 'applied'
    } else {
      state.toolsSuggestion = 'notApplied'
    }
  }
  if (result.vision.ran && result.vision.supported === true) {
    if (row.inMods === null) {
      row.inMods = [...PROBE_SUGGESTED_IN_MODS]
      state.visionSuggestion = 'applied'
    } else if (sameModalitySet(row.inMods, PROBE_SUGGESTED_IN_MODS) && prev?.visionSuggestion === 'applied') {
      // Same no-downgrade rule as tools above: the set is exactly what the
      // last probe wrote and no edit has retired it, so it stays applied.
      state.visionSuggestion = 'applied'
    } else {
      state.visionSuggestion = 'notApplied'
    }
  }
  probeStates.value = { ...probeStates.value, [row.name]: state }
}

// A suggestion only vouches for the moment it was made: the badge names the
// value the probe wrote, and the kept-hint describes the declaration the
// probe chose not to overwrite. A later hand edit can invalidate either —
// the badge would vouch for a value that is no longer in the field, the hint
// would describe a declaration that no longer exists (cleared away, or
// extended until it now states what the probe suggested) — so every manual
// edit of the two suggested fields re-checks the state and silently drops the
// suggestion that no longer holds. (The backfill above writes the row
// directly and never passes through here on purpose: it must not retire the
// suggestion it just recorded.)
function syncSuggestionStateAfterEdit(row: ImportRow) {
  const state = probeStates.value[row.name]
  if (!state) return
  if (
    (state.visionSuggestion === 'applied' && !sameModalitySet(row.inMods, PROBE_SUGGESTED_IN_MODS)) ||
    // notApplied retires when the declaration it describes is gone: cleared
    // to undeclared, or extended by hand until it includes image — the hint's
    // own ask ("consider adding Image") has then been carried out.
    (state.visionSuggestion === 'notApplied' &&
      (row.inMods === null || row.inMods.length === 0 || row.inMods.includes('image')))
  ) {
    state.visionSuggestion = ''
  }
  if (
    (state.toolsSuggestion === 'applied' && row.tools !== 1) ||
    // Same retirement for tools: undeclared again (0), or flipped by hand to
    // supported (1) — exactly the value the probe would have suggested, so
    // "your declaration was left unchanged" no longer describes the form.
    (state.toolsSuggestion === 'notApplied' && (row.tools === 0 || row.tools === 1))
  ) {
    state.toolsSuggestion = ''
  }
}

// Order-insensitive on purpose: the picker reordering a multi-select (or a
// tag removed and re-added) does not change what is declared.
function sameModalitySet(a: string[] | null, b: string[]): boolean {
  if (a === null || a.length !== b.length) return false
  const others = new Set(b)
  return a.every((mod) => others.has(mod))
}

// The conclusion bar's four entries. The basic probe is binary, the two
// capability probes tri-state, and the vision arm tri-state with its own
// "unknown" word for an unclaimed verdict. A skipped probe reads "not
// tested" (—), never as a failure; a failed one names its outcome category
// (the shared word list) instead of a bare ✗.
type ProbeBarTone = 'ok' | 'bad' | 'unknown' | 'untested'

interface ProbeBarEntry {
  label: string
  icon: string
  tone: ProbeBarTone
  note: string
}

function outcomeNote(p: ProbeReport): string {
  return p.outcome === null || p.outcome === undefined ? '' : testOutcomeLabel(t, p.outcome)
}

function basicBarEntry(p: ProbeReport): ProbeBarEntry {
  const label = t('models.basicText')
  if (!p.ran) return { label, icon: '—', tone: 'untested', note: t('models.probeNotTested') }
  if (p.supported === true) return { label, icon: '✓', tone: 'ok', note: t('models.testPassed') }
  return { label, icon: '✗', tone: 'bad', note: outcomeNote(p) || t('models.testFailed') }
}

function capabilityBarEntry(label: string, p: ProbeReport): ProbeBarEntry {
  if (!p.ran) return { label, icon: '—', tone: 'untested', note: t('models.probeNotTested') }
  if (p.supported === true) return { label, icon: '✓', tone: 'ok', note: t('models.probeConfirmed') }
  if (p.supported === false) return { label, icon: '✗', tone: 'bad', note: outcomeNote(p) || t('models.probeUnsupported') }
  return { label, icon: '?', tone: 'unknown', note: t('models.probeUnconfirmed') }
}

function visionBarEntry(p: ProbeReport): ProbeBarEntry {
  const label = t('models.importProbeVisionLabel')
  if (!p.ran) return { label, icon: '—', tone: 'untested', note: t('models.probeNotTested') }
  if (p.supported === true) return { label, icon: '✓', tone: 'ok', note: '' }
  if (p.supported === false) return { label, icon: '✗', tone: 'bad', note: outcomeNote(p) }
  return { label, icon: '?', tone: 'unknown', note: t('models.importProbeVisionUnknown') }
}

const probeBarEntries = computed<ProbeBarEntry[]>(() => {
  const st = selectedProbeState.value
  if (!st) return []
  const r = st.result.report
  return [
    basicBarEntry(r.basic),
    capabilityBarEntry(t('models.supportsStreaming'), r.streaming),
    capabilityBarEntry(t('models.supportsFunctionCalling'), r.function_calling),
    visionBarEntry(st.result.vision),
  ]
})

const probeTotalMs = computed(() => {
  const st = selectedProbeState.value
  if (!st) return 0
  const r = st.result.report
  return r.basic.duration_ms + r.streaming.duration_ms + r.function_calling.duration_ms + st.result.vision.duration_ms
})

// The failing arms' refusals in the upstream's own words: the basic arm
// whenever it ran and did not succeed — for it the detail is often the ONLY
// reason available (the no-usable-key path has no other explanation to
// show) — and the vision arm whenever it ran and did not confirm vision,
// "not supported" and "unknown" alike. Basic renders first; an arm that
// passed, was skipped, or carries no detail contributes nothing.
const failureDetails = computed<string[]>(() => {
  const st = selectedProbeState.value
  if (!st) return []
  const details: string[] = []
  const b = st.result.report.basic
  if (b.ran && b.supported !== true && b.detail) details.push(b.detail)
  const v = st.result.vision
  if (v.ran && v.supported !== true && v.detail) details.push(v.detail)
  return details
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
  probeStates.value = {}
  // The in-flight flag is session state like the rows: a probe left running
  // when the dialog closed must not pin the fresh session's button on
  // "Probing…" — the old promise may never land, and its guarded finally (see
  // onProbe) deliberately no longer clears anything, so reset owns the clear.
  probeRunning.value = false
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

/* The probe's conclusion bar: one line, one verdict per probe. Icon colour
   carries the tone (✓ green, ✗ red, ? amber, — muted) so the words can stay
   small; the whole strip reads as one status region (role="status"). */
.import-detail__probe-report {
  display: flex;
  align-items: baseline;
  gap: var(--space-3);
  flex-wrap: wrap;
  margin-top: var(--space-3);
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-md);
  background: var(--color-bg-soft);
  font-size: var(--text-xs);
}

.import-detail__probe-report__title {
  color: var(--color-text-secondary);
  font-weight: 600;
}

.import-detail__probe-report__entry {
  display: inline-flex;
  align-items: baseline;
  gap: 4px;
}

.import-detail__probe-report__icon--ok {
  color: var(--color-success);
}

.import-detail__probe-report__icon--bad {
  color: var(--color-danger);
}

.import-detail__probe-report__icon--unknown {
  color: var(--color-warning);
}

.import-detail__probe-report__icon--untested {
  color: var(--color-text-muted);
}

.import-detail__probe-report__note,
.import-detail__probe-report__duration {
  color: var(--color-text-secondary);
}

/* A suggestion the probe made but did NOT write (the row already declared the
   capability) — a prose line under the report bar. */
.import-detail__suggestion {
  margin: var(--space-2) 0 0;
  color: var(--color-text-secondary);
  font-size: var(--text-xs);
}

/* The "probe suggested" badge sits beside a field label it vouches for. */
.import-field__suggested {
  margin-left: 4px;
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
