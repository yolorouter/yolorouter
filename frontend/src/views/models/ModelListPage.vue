<!-- frontend/src/views/models/ModelListPage.vue
     The models list: one full fetch, client-side filters (name substring,
     running / management status, provider, unpriced-only) over the applied
     snapshot, client pagination, and the expandable per-model price
     comparison. Bulk pricing is a desktop capability merged in from the
     standalone pricing page: a leading selection column whose header
     select-all covers the CURRENT FILTERED models (a Set of model ids that
     persists across paging and filter sweeps), a count-labeled toolbar
     entry, and the two-phase drawer — operation chooser, then the dry-run
     preview whose confirm applies to the deduped union of the selected
     models' candidates, reloads, clears the selection, and summarizes.
     Every refetch this page initiates lands in one choke-point that
     clears the selection (model creations refetch inside the create
     modal's store action and only ever ADD rows, so they are left
     alone), and the drawer refuses to open when that union is empty.
     Below the shared 768px breakpoint none of that renders: browse, filter,
     expand, and single-row edit stay available. -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('models.eyebrow')" :title="t('models.pageTitle')" :description="t('models.pageDescription')">
      <template #actions>
        <n-button @click="showVisionFallback = true">
          {{ t('models.visionFallbackButton') }}
        </n-button>
        <n-button type="primary" @click="showCreate = true">
          <template #icon><Plus :size="16" /></template>
          {{ t('models.createButton') }}
        </n-button>
      </template>
    </PageHeader>

    <EmptyState v-if="!store.loading && store.list.length === 0" :icon="Boxes" :title="t('models.listEmpty')">
      <template #action>
        <n-button type="primary" @click="showCreate = true">{{ t('models.createButton') }}</n-button>
      </template>
    </EmptyState>

    <template v-else>
      <div class="filter-panel">
        <div class="filter-grid">
          <div class="filter-item filter-item--search">
            <n-input
              v-model:value="filter.name"
              :placeholder="t('models.filterName')"
              clearable
              size="small"
              @keyup.enter="onSearch"
            >
              <template #prefix><Search :size="14" /></template>
            </n-input>
          </div>
          <FilterSelectField
            v-model:value="filter.running"
            :label="t('models.filterRunningStatus')"
            :options="runningStatusOptions"
            :placeholder="t('models.filterRunningStatus')"
            size="small"
            width="100%"
            @update:value="onSearch"
          />
          <FilterSelectField
            v-model:value="filter.management"
            :label="t('models.filterManagementStatus')"
            :options="managementStatusOptions"
            :placeholder="t('models.filterManagementStatus')"
            size="small"
            width="100%"
            @update:value="onSearch"
          />
          <FilterSelectField
            v-model:value="filter.provider"
            :label="t('models.filterProvider')"
            :options="providerOptions"
            :placeholder="t('models.filterProvider')"
            filterable
            size="small"
            width="100%"
            @update:value="onSearch"
          />
          <!-- The switch applies immediately, like the selects beside it: the
               handler writes the draft, then snapshots it into the applied
               filter the table reads. -->
          <label class="filter-item filter-switch">
            <n-switch size="small" :value="filter.unpricedOnly" @update:value="onUnpricedOnlyChange" />
            <span>{{ t('models.filterUnpricedOnly') }}</span>
          </label>
          <div class="filter-actions">
            <!-- The bulk entry travels with the selection layer: desktop only,
                 disabled until at least one model row is ticked. -->
            <n-button
              v-if="!isMobile"
              size="small"
              type="warning"
              ghost
              class="models-bulk-button"
              :disabled="selectedModelIds.size === 0"
              @click="openBulk"
            >
              <template #icon><BadgePercent :size="14" /></template>
              {{ bulkButtonLabel }}
            </n-button>
            <n-button size="small" type="primary" @click="onSearch">{{ t('models.search') }}</n-button>
            <n-button size="small" quaternary @click="onReset">{{ t('models.reset') }}</n-button>
          </div>
        </div>
      </div>

      <div class="data-table-wrapper">
        <ResponsiveDataTable
          :columns="columns"
          :data="filteredModels"
          :loading="store.loading"
          :scroll-x="1100"
          :row-key="(row: Model) => row.id"
          :row-props="rowProps"
          :full-span-keys="['expand_candidates']"
          :pagination="pagination"
        />
      </div>
    </template>

    <NewModelModal v-model:show="showCreate" />
    <ModelEditModal v-model:show="showEditModel" :model="editingModel" @updated="onEdited" />
    <!-- The shared candidate edit modal the mapping tables use: opened on a
         comparison row with THAT model (the PATCH URL's owner) and the row
         itself; saved applies the same PATCH write path, then the list
         refreshes and the expansion re-renders from stored prices. -->
    <CandidateEditModal
      v-model:show="showEditCandidate"
      :model-id="editingCandidateRow?.model.id ?? 0"
      :model-name="editingCandidateRow?.model.name ?? ''"
      :editing-candidate="editingCandidateRow?.candidate ?? null"
      @saved="onCandidateEdited"
    />
    <DeleteModelModal v-model:show="showDeleteModel" :model="deletingModel" @deleted="onModelDeleted" />
    <VisionFallbackModal v-model:show="showVisionFallback" />
    <CCSwitchImportModal
      v-model:show="showCCSImport"
      mode="key"
      :model-row="ccsModelRow"
      :owner-username="authStore.username ?? ''"
      @confirm="onCCSConfirm"
    />

    <!-- The desktop bulk-price flow, two phases in one drawer (the pricing
         page's shape): the operation chooser, then the dry-run preview.
         Confirm on phase one previews (dry_run true); confirm on phase two
         applies (dry_run false). Cancel / mask / Esc close at any point
         without ever sending the write — except while a request is in
         flight: the dismissable lock withholds every dismiss affordance for
         the preview fetch and seals the apply write's point of no return
         against cancel / mask / Esc. -->
    <ModalDrawer
      v-model:show="bulkShow"
      :title="bulkTitle"
      max-width="640px"
      :confirm-text="bulkConfirmText"
      :cancel-text="t('common.cancel')"
      :back-label="t('common.back')"
      :loading="bulkBusy"
      :dismissable="!bulkBusy"
      :confirm-disabled="!bulkCanConfirm"
      @confirm="onBulkConfirm"
      @after-leave="onBulkClosed"
    >
      <div v-if="bulkPhase === 'choose'" class="bulk-choose">
        <div class="bulk-choose__field">
          <span class="bulk-choose__label">{{ t('models.bulkOperationLabel') }}</span>
          <n-radio-group v-model:value="bulkOperation" name="bulk-operation">
            <n-radio value="scale">{{ t('models.bulkOperationScale') }}</n-radio>
            <n-radio value="apply_suggested">{{ t('models.bulkOperationApplySuggested') }}</n-radio>
          </n-radio-group>
          <p class="bulk-choose__hint">
            {{ bulkOperation === 'scale' ? t('models.bulkOperationScale_tip') : t('models.bulkOperationApplySuggested_tip') }}
          </p>
        </div>
        <div v-if="bulkOperation === 'scale'" class="bulk-choose__field">
          <HelpLabel :tip="t('models.bulkFactorLabel_tip')">{{ t('models.bulkFactorLabel') }}</HelpLabel>
          <n-input-number
            v-model:value="bulkFactor"
            class="bulk-factor"
            :min="0"
            :step="0.01"
            :placeholder="t('models.bulkFactorPlaceholder')"
          />
          <p v-if="factorInvalid" class="bulk-choose__error">{{ t('models.bulkFactorInvalid') }}</p>
        </div>
      </div>
      <div v-else-if="bulkPreview" class="bulk-preview">
        <p class="bulk-preview__summary">
          {{ t('models.bulkPreviewSummary', { applied: bulkPreview.applied, skipped: bulkPreview.skipped }) }}
        </p>
        <p v-if="bulkPreview.applied === 0" class="bulk-preview__none">{{ t('models.bulkPreviewNothingToApply') }}</p>
        <div
          v-for="item in bulkPreview.items"
          :key="item.candidate_id"
          class="bulk-preview__item"
          :class="{ 'bulk-preview__item--skipped': !item.changed }"
        >
          <div class="bulk-preview__head">
            <span class="bulk-preview__name">{{ item.model_name }} · {{ item.provider_name }}</span>
            <n-tag size="tiny" :bordered="false" :type="item.changed ? 'success' : 'warning'">
              {{ item.changed ? t('models.bulkChangedTag') : t('models.bulkSkippedTag') }}
            </n-tag>
          </div>
          <div class="bulk-preview__detail">
            <template v-if="item.changed">
              <span v-for="change in itemSlotChanges(item)" :key="change.labelKey" class="bulk-preview__slot">
                {{ t(change.labelKey) }} {{ change.before }} → {{ change.after }}
              </span>
            </template>
            <span v-else class="bulk-preview__reason">{{ skipReasonLabel(item.skip_reason) }}</span>
          </div>
        </div>
      </div>
    </ModalDrawer>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
// NCheckbox / NRadio / NRadioGroup are NOT in main.ts's create() components
// list — without these imports Vue renders them as unknown elements (the
// typecheck and build stay green while the UI breaks).
import { NButton, NCheckbox, NRadio, NRadioGroup, NSwitch, NTag, useDialog, useMessage, type DataTableColumns } from 'naive-ui'
import { BadgePercent, Boxes, Plus, Search, MoreHorizontal } from '@lucide/vue'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { useConfirmedStatusToggle } from '../../composables/useConfirmedStatusToggle'
import { modelDisableCopy } from '../../utils/impactSummary'
import { modelRunningStatusDisplay, MODEL_RUNNING_STATUS_DISPLAY } from '../../utils/modelStatusDisplay'
import { modalityBadge, modalityBadgeLabel } from '../../utils/modalityBadge'
import { columnTitle } from '../../utils/columnTitle'
import { isBalancedModel } from '../../utils/schedulingMode'
import { modelCostDetailLocation } from '../../utils/modelCostLocation'
import { useClientPagination } from '../../composables/useClientPagination'
import { useIsMobile } from '../../composables/useIsMobile'
import { expandPanel, EXPAND_EMPTY_STYLE, rowNavigationProps } from '../../utils/expandPanel'
import { comparisonRows, cheapestComparisonRow, modelHasUnpriced, modelPriceMetrics } from './modelPriceSummary'
import { PriceLinesBlock, ProbeTagBlock } from './pricingRowBlocks'
import { previewSlotChanges, selectionStatus, toggleRow } from './pricingBulkSelection'
import CandidateUnpricedTag from '../../components/models/CandidateUnpricedTag.vue'
import type { VNodeChild } from 'vue'
import type { BulkPriceItem, BulkPriceOperation, BulkPricePayload, BulkPriceResult, Model, ModelCandidate } from '../../api/models'
import { postBulkPrice } from '../../api/models'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import NewModelModal from '../../components/models/NewModelModal.vue'
import ModelEditModal from '../../components/models/ModelEditModal.vue'
import CandidateEditModal from '../../components/models/CandidateEditModal.vue'
import DeleteModelModal from '../../components/models/DeleteModelModal.vue'
import VisionFallbackModal from '../../components/models/VisionFallbackModal.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import ResponsiveDropdown from '../../components/common/ResponsiveDropdown.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import ModalDrawer from '../../components/common/ModalDrawer.vue'
import { ccsProfileName } from '../../utils/format'
import { useCCSwitchImport } from '../../composables/useCCSwitchImport'
import { useRowModal } from '../../composables/useRowModal'
import { useAuthStore } from '../../store/auth'
import CCSwitchImportModal from '../../components/ccswitch/CCSwitchImportModal.vue'
import type { CCSwitchConfirmPayload } from '../../utils/ccswitchExport'

const { t } = useI18n()
const isMobile = useIsMobile()
const route = useRoute()
const router = useRouter()
const dialog = useDialog()
const toggleStatusWithConfirm = useConfirmedStatusToggle(dialog)
const message = useMessage()
const store = useModelsStore()
const { importToCCS } = useCCSwitchImport()

// --- CC-Switch export (key mode) -------------------------------------------
// The row action opens the shared export dialog in key mode: the model is
// fixed, the viewer picks one of THEIR OWN compatible keys, and the dialog
// prefetched that key's plaintext before Confirm arms. The confirm payload
// reaches the deep link here, synchronously in the click's own handler.
const authStore = useAuthStore()
const { row: ccsModelRow, show: showCCSImport } = useRowModal<Model>()

function openCCSImport(row: Model) {
  ccsModelRow.value = row
}

function onCCSConfirm(payload: CCSwitchConfirmPayload) {
  const row = ccsModelRow.value
  if (!row) return
  importToCCS({ name: ccsProfileName(row.name), apiKey: payload.apiKey, model: row.name })
}
const showCreate = ref(false)
const showVisionFallback = ref(false)
// Inline row edit: reuse the same edit modal the detail page uses, opened
// straight from the list so a name change needs no navigation.
const showEditModel = ref(false)
const editingModel = ref<Model | null>(null)

function openEditModel(row: Model) {
  editingModel.value = row
  showEditModel.value = true
}

function onEdited() {
  void reload().catch((err) => message.error(displayMessage(err, t)))
}

// Inline candidate edit from the comparison expansion: the shared modal the
// mapping tables use, opened with the row's model (the PATCH URL's owner) and
// the candidate itself. A saved modal refetches the list, so the expansion
// re-renders from stored prices.
const showEditCandidate = ref(false)
const editingCandidateRow = ref<{ model: Model; candidate: ModelCandidate } | null>(null)

function openCandidateEdit(model: Model, candidate: ModelCandidate) {
  editingCandidateRow.value = { model, candidate }
  showEditCandidate.value = true
}

function onCandidateEdited() {
  void reload().catch((err) => message.error(displayMessage(err, t)))
}

// Row delete: assigning the row opens the modal (useRowModal binds show to
// the row). After a successful delete the list refreshes in place — the row
// simply leaves the table, filters and pagination keep their state.
const { row: deletingModel, show: showDeleteModel } = useRowModal<Model>()

function onModelDeleted() {
  void reload().catch((err) => message.error(displayMessage(err, t)))
}

// In-page filters over the fully-fetched list (name substring + running /
// management status + unpriced-only). `filter` is the live draft the inputs
// edit; `applied` is the snapshot the table actually filters by — mirroring
// the search/reset flow on the request-logs page so the text field only takes
// effect on Enter / the Search button, not on every keystroke.
interface ModelFilter {
  name: string
  running: string | null
  management: number | null
  provider: number | null
  unpricedOnly: boolean
}
const emptyFilter = (): ModelFilter => ({ name: '', running: null, management: null, provider: null, unpricedOnly: false })
const filter = reactive<ModelFilter>(emptyFilter())
const applied = reactive<ModelFilter>(emptyFilter())

const runningStatusOptions = computed(() =>
  Object.entries(MODEL_RUNNING_STATUS_DISPLAY).map(([value, { i18nKey }]) => ({
    label: t(`models.running${i18nKey}`),
    value,
  })),
)
const managementStatusOptions = computed(() => [
  { label: t('models.statusEnabled'), value: 1 },
  { label: t('models.statusDisabled'), value: 2 },
])

// The provider filter answers "which models route through this provider" —
// options are the union of providers across every model's candidates, so
// only providers that actually appear somewhere are offered.
const providerOptions = computed(() => {
  const byId = new Map<number, string>()
  for (const m of store.list) {
    for (const c of m.candidates) {
      if (!byId.has(c.provider_id)) byId.set(c.provider_id, c.provider_name)
    }
  }
  return [...byId.entries()]
    .map(([value, label]) => ({ label, value }))
    .sort((a, b) => a.label.localeCompare(b.label))
})

// A selected provider can drop out of the options when its last candidate
// goes away and the list refreshes. Left in place it becomes a ghost filter:
// a blank select over an empty table with nothing explaining why.
watch(providerOptions, (opts) => {
  const known = (v: number | null) => v === null || opts.some((o) => o.value === v)
  if (!known(filter.provider)) filter.provider = null
  if (!known(applied.provider)) applied.provider = null
})

const filteredModels = computed(() => {
  const q = applied.name.trim().toLowerCase()
  return store.list.filter((m) => {
    if (q && !m.name.toLowerCase().includes(q)) return false
    if (applied.running && m.running_status !== applied.running) return false
    if (applied.management !== null && m.management_status !== applied.management) return false
    if (applied.provider !== null && !m.candidates.some((c) => c.provider_id === applied.provider)) return false
    // Model-level unpriced filter: the model matches when ANY of its
    // candidates is unpriced (the shared predicate's single verdict).
    if (applied.unpricedOnly && !modelHasUnpriced(m.candidates)) return false
    return true
  })
})

// Client-side pagination: models are few (admin-configured), so the full list
// is fetched once and sliced in the table rather than adding a server-side
// paged endpoint.
const { pagination } = useClientPagination()

// A narrowed filter can leave the current page past the end of the results —
// reset to the first page whenever a filter is applied.
function onSearch() {
  Object.assign(applied, filter)
  pagination.page = 1
}
function onReset() {
  Object.assign(filter, emptyFilter())
  Object.assign(applied, emptyFilter())
  pagination.page = 1
}

// The switch applies immediately, like the selects beside it: write the
// draft, then snapshot it into the applied filter.
function onUnpricedOnlyChange(value: boolean) {
  filter.unpricedOnly = value
  onSearch()
}

// --- Bulk pricing selection (desktop capability; mobile renders none of it) -

// The ticked MODEL ids. Selection is model-grained: ticking a model puts ALL
// of its candidates into the batch. The pure algebra (the tri-state status,
// the row toggle) comes from the shared pricingBulkSelection module; the flat
// select-all below is the same Set sweep inline — the table has no groups.
const selectedModelIds = ref<Set<number>>(new Set())

// Choke-point for the list refetches this page initiates (post-edit,
// post-candidate-save, post-delete, post-toggle, and the initial mount —
// NOT the create modal's internal refetch, which only adds rows): the
// bulk selection names MODEL ids against the list as it WAS, so a selection
// never outlives the refetch — the fresh list may have reordered or dropped
// the ticked models, and a stale selection would inflate the toolbar count
// with ghost ids or send an empty candidate union to the bulk endpoint (a
// guaranteed 400). The next batch must be an explicit choice against current
// data.
async function reload(): Promise<void> {
  await store.fetchList()
  selectedModelIds.value = new Set()
}

// The select-all reads the VISIBLE scope: the currently filtered models (the
// applied snapshot), across every page — the batch is what the admin sees
// under the active filter, not merely the rows on screen.
const selectAllStatus = computed(() => selectionStatus(filteredModels.value.map((m) => m.id), selectedModelIds.value))

function onSelectAll(checked: boolean) {
  const next = new Set(selectedModelIds.value)
  for (const model of filteredModels.value) {
    if (checked) next.add(model.id)
    else next.delete(model.id)
  }
  selectedModelIds.value = next
}

function onModelCheck(modelId: number) {
  selectedModelIds.value = toggleRow(modelId, selectedModelIds.value)
}

const bulkButtonLabel = computed(() =>
  selectedModelIds.value.size === 0
    ? t('models.bulkButton')
    : t('models.bulkButtonWithCount', { count: selectedModelIds.value.size }),
)

// The deduped union of ALL candidates of the selected models — the bulk
// endpoint's candidate_ids. Built from the store list, so a model deleted
// after its row was ticked silently drops out of the batch.
const selectedCandidateIds = computed<number[]>(() => {
  const ids = new Set<number>()
  for (const model of store.list) {
    if (!selectedModelIds.value.has(model.id)) continue
    for (const candidate of model.candidates) ids.add(candidate.id)
  }
  return [...ids]
})

// Every fetched candidate by id: the preview's billing-mode lookup (the
// endpoint reports slot snapshots, not each row's billing mode).
const candidateById = computed<Map<number, ModelCandidate>>(() => {
  const byId = new Map<number, ModelCandidate>()
  for (const model of store.list) {
    for (const candidate of model.candidates) byId.set(candidate.id, candidate)
  }
  return byId
})

// --- Bulk flow: chooser -> dry-run preview -> apply --------------------------

const bulkShow = ref(false)
const bulkPhase = ref<'choose' | 'preview'>('choose')
const bulkOperation = ref<BulkPriceOperation>('scale')
// number | null is what the input number emits; null = untouched, and only a
// value ABOVE zero passes (zero and negatives are rejected client-side).
const bulkFactor = ref<number | null>(null)
const bulkPreview = ref<BulkPriceResult | null>(null)
const bulkBusy = ref(false)

// Monotonic session token for the bulk flow: every open and close bumps it,
// and each in-flight request captures its value before awaiting, proceeding
// after the await only if the token still matches. A stale response — one
// that lands after a close, or after a close-and-reopen started a fresh
// session — is retired instead of writing preview state, toasting, or
// reloading against a drawer the user is no longer in.
let bulkReqSeq = 0

const factorInvalid = computed(() => bulkOperation.value === 'scale' && bulkFactor.value !== null && bulkFactor.value <= 0)

const bulkTitle = computed(() => (bulkPhase.value === 'choose' ? t('models.bulkDialogTitle') : t('models.bulkPreviewTitle')))

const bulkConfirmText = computed(() => (bulkPhase.value === 'choose' ? t('models.bulkPreviewButton') : t('models.bulkConfirmApply')))

// Choose phase: scale needs a positive factor, apply_suggested nothing.
// Preview phase: at least one row must be applicable for a write to mean
// anything (an all-skipped preview has nothing to confirm).
const bulkCanConfirm = computed(() => {
  if (bulkPhase.value === 'choose') {
    return bulkOperation.value === 'apply_suggested' || (bulkFactor.value !== null && bulkFactor.value > 0)
  }
  return (bulkPreview.value?.applied ?? 0) > 0
})

// The endpoint binds candidate_ids to 1..2000 raw: a union past the cap would
// 400 no matter what the body says, so it is refused at the door with the
// numbers spelled out and the drawer stays closed.
const BULK_PRICE_MAX_IDS = 2000

function openBulk() {
  if (selectedModelIds.value.size === 0) return
  const union = selectedCandidateIds.value
  // The selection names models, but the batch is candidate ids: a model with
  // no candidates (or ids left ticked that no longer contribute anything)
  // would post an empty union and earn the endpoint's 400. Refused at the
  // same door as the cap, with the drawer never opening.
  if (union.length === 0) {
    message.warning(t('models.bulkNothingSelected'))
    return
  }
  if (union.length > BULK_PRICE_MAX_IDS) {
    message.warning(t('models.bulkOverCap', { count: union.length, cap: BULK_PRICE_MAX_IDS }))
    return
  }
  // A reopen starts a NEW session: the bump retires any bulk request still
  // in flight from the previous one, so its late response can never write
  // preview state into — or act against — a drawer the user has already
  // left and re-entered.
  bulkReqSeq++
  bulkShow.value = true
  // A reopen always starts at the chooser: anything a half-lived flow left
  // behind is cleared here rather than inherited.
  bulkPhase.value = 'choose'
  bulkPreview.value = null
  bulkFactor.value = null
}

// The factor key belongs to scale only: for apply_suggested it stays OFF the
// payload object entirely — the backend rejects any NON-NULL factor there (a
// JSON null would decode into its nil pointer and slip through), and keeping
// the key absent is the unambiguous shape rather than a tolerated accident.
function bulkPayload(dryRun: boolean): BulkPricePayload {
  const payload: BulkPricePayload = {
    candidate_ids: selectedCandidateIds.value,
    operation: bulkOperation.value,
    dry_run: dryRun,
  }
  if (bulkOperation.value === 'scale') payload.factor = bulkFactor.value!
  return payload
}

async function onBulkConfirm() {
  if (bulkBusy.value || !bulkCanConfirm.value || selectedModelIds.value.size === 0) return
  if (bulkPhase.value === 'choose') {
    bulkBusy.value = true
    const seq = ++bulkReqSeq
    try {
      const result = await postBulkPrice(bulkPayload(true))
      // The drawer can close — or close and reopen — while the dry run is in
      // flight. The token retires the response in both cases: a stale result
      // captures nothing, so it cannot flip a closed drawer into a preview
      // or skip the chooser on a reopened one.
      if (seq !== bulkReqSeq) return
      bulkPreview.value = result
      bulkPhase.value = 'preview'
    } catch (err) {
      // Same staleness rule on the failure path: an abandoned session's
      // error is never toasted against the session that replaced it.
      if (seq !== bulkReqSeq) return
      message.error(displayMessage(err, t))
    } finally {
      // Only the live session releases the busy lock: a retired request's
      // finally must not unstick a newer session's in-flight spinner (that
      // would re-arm the confirm button mid-request — a double-apply
      // window). No stuck-lock risk: onBulkClosed clears it unconditionally.
      if (seq === bulkReqSeq) bulkBusy.value = false
    }
    return
  }
  // The write. Closing the drawer first keeps a slow apply from reading as a
  // hung dialog; a failure reopens nothing — the summary stays in the toast.
  bulkBusy.value = true
  const seq = ++bulkReqSeq
  try {
    const result = await postBulkPrice(bulkPayload(false))
    // The apply can be abandoned mid-flight (cancel, mask, Esc, then a
    // reopen): a stale success must not close the drawer, toast, clear the
    // selection, or reload against the fresh session, and a stale failure
    // must not toast either.
    if (seq !== bulkReqSeq) return
    bulkShow.value = false
    message.success(t('models.bulkAppliedSummary', { applied: result.applied, skipped: result.skipped }))
    // The write already happened: drop the selection and refresh even if the
    // refresh itself fails (its error is surfaced on its own).
    selectedModelIds.value = new Set()
    try {
      await store.fetchList()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  } catch (err) {
    // The apply was refused: keep the preview up so the flow can be retried
    // or abandoned with the eyes-on evidence still there. A stale failure is
    // retired by the token check — its toast belongs to the live session
    // only.
    if (seq !== bulkReqSeq) return
    message.error(displayMessage(err, t))
  } finally {
    // Same rule as the preview path: the lock belongs to the live session
    // only (this finally can land well after a close-and-reopen — the
    // reload above keeps this frame alive past onBulkClosed).
    if (seq === bulkReqSeq) bulkBusy.value = false
  }
}

// Anything that closes the drawer (cancel, mask, Esc, mobile back arrow)
// lands here: no write has been sent on the preview path, and the next open
// starts clean at the chooser.
function onBulkClosed() {
  // Retire in-flight bulk requests at close time: between the leave
  // finishing and the next open, no late response has a live session to act
  // on.
  bulkReqSeq++
  bulkPhase.value = 'choose'
  bulkOperation.value = 'scale'
  bulkFactor.value = null
  bulkPreview.value = null
  bulkBusy.value = false
}

// The skip reasons the backend sends, each to its own bilingual label.
const SKIP_REASON_KEYS: Record<NonNullable<BulkPriceItem['skip_reason']>, string> = {
  not_found: 'models.bulkSkipNotFound',
  unpriced: 'models.bulkSkipUnpriced',
  unsupported_billing_mode: 'models.bulkSkipUnsupportedBillingMode',
  no_suggestion: 'models.bulkSkipNoSuggestion',
  unchanged: 'models.bulkSkipUnchanged',
}

function skipReasonLabel(reason: BulkPriceItem['skip_reason']): string {
  return reason && SKIP_REASON_KEYS[reason] ? t(SKIP_REASON_KEYS[reason]) : t('models.bulkSkippedTag')
}

// A changed item's before→after entries, family per the row's billing mode
// (the endpoint reports slot snapshots; the page knows the mode).
function itemSlotChanges(item: BulkPriceItem) {
  return previewSlotChanges(candidateById.value.get(item.candidate_id)?.billing_mode, item.before, item.after)
}

onMounted(() => {
  // Deep links: ?management=1|2 (the dashboard's unavailable-models figure
  // counts disabled models, so its drill-down filters this dimension) and
  // ?running=<running status>. Other filters have no inbound links today;
  // add their keys here when one appears.
  const management = route.query.management
  if (management === '1' || management === '2') {
    filter.management = Number(management)
    applied.management = Number(management)
  }
  const running = route.query.running
  if (typeof running === 'string' && Object.hasOwn(MODEL_RUNNING_STATUS_DISPLAY, running)) {
    filter.running = running
    applied.running = running
  }
  void reload().catch((err) => message.error(displayMessage(err, t)))
})

function goDetail(id: number) {
  router.push(`/models/${id}`)
}

function rowProps(row: Model) {
  return rowNavigationProps(() => goDetail(row.id))
}

// The compare rows' shared track template and the header cell style. One
// const for both the header and the rows: the labels ride the exact tracks
// they name, so a track edit can never leave the header drifting off the
// columns. Tracks are FIXED (not fr/max-content) on purpose — every row is
// its own grid, and content-sized tracks would give long provider names or
// the cheapest-only tag their own column widths, breaking the shared column
// axis; the badge track reserves the widest tag combo so the edit button
// cannot drift either.
const COMPARE_ROW_STYLE =
  'display:grid; grid-template-columns:130px 210px minmax(160px,1fr) 240px max-content; column-gap:12px; align-items:center;'
const COMPARE_HEADER_CELL_STYLE =
  'font-size:var(--text-xs); color:var(--color-text-muted); white-space:nowrap; overflow:hidden; text-overflow:ellipsis;'

// The expanded comparison surface: the model's candidates as price-sorted
// rows (the pricing page's row order via comparisonRows), the cheapest priced
// row highlighted with the cheapest tag, each row carrying the shared price
// lines, probe badge, and unpriced badge, plus an edit entry into the shared
// CandidateEditModal. The expand trigger mechanics are the table's; only the
// panel content changed from the old route-order grid. Inline styles because
// scoped CSS does not reach h()-rendered nodes.
function renderModelExpand(row: Model) {
  if (!row.candidates.length) {
    return expandPanel({ eyebrow: isMobile.value ? undefined : t('models.expandComparison'), indent: !isMobile.value }, [
      h('div', { style: EXPAND_EMPTY_STYLE }, t('models.expandNoCandidates')),
    ])
  }
  const rows = comparisonRows(row)
  const cheapest = cheapestComparisonRow(rows)
  const header = h(
    'div',
    { class: 'model-compare-header', style: COMPARE_ROW_STYLE + 'padding:0 10px 2px;' },
    [
      t('models.compareProvider'),
      t('models.providerModelName'),
      t('models.priceColumn'),
      t('models.compareStatus'),
      t('common.actions'),
    ].map((label) => h('span', { style: COMPARE_HEADER_CELL_STYLE }, label)),
  )
  const list = h(
    'div',
    { style: 'display:flex; flex-direction:column; gap:6px;' },
    [
      header,
      ...rows.map(({ candidate }) => {
      const isCheapest = cheapest !== null && cheapest.candidate.id === candidate.id
      return h(
        'div',
        {
          key: candidate.id,
          class: isCheapest ? 'model-compare-row model-compare-row--cheapest' : 'model-compare-row',
          'data-candidate-id': String(candidate.id),
          style:
            COMPARE_ROW_STYLE + ' padding:6px 10px;' +
            ' border:1px solid var(--color-border-subtle); border-radius:var(--radius-md);' +
            (isCheapest ? ' background:rgba(82,196,26,0.08); border-color:rgba(82,196,26,0.35);' : ''),
        },
        [
          h(
            'span',
            { title: candidate.provider_name, style: 'font-size:var(--text-sm); font-weight:500; color:var(--color-text); overflow:hidden; text-overflow:ellipsis; white-space:nowrap;' },
            candidate.provider_name,
          ),
          candidate.provider_model_name
            ? h(
                'span',
                { title: candidate.provider_model_name, style: 'font-family:var(--font-mono); font-size:var(--text-xs); color:var(--color-text-secondary); overflow:hidden; text-overflow:ellipsis; white-space:nowrap;' },
                candidate.provider_model_name,
              )
            : h('span', { style: 'font-size:var(--text-xs); color:var(--color-text-muted);' }, '—'),
          h('div', [h(PriceLinesBlock, { candidate })]),
          h(
            'div',
            { style: 'display:flex; align-items:center; justify-content:flex-start; gap:6px; flex-wrap:wrap;' },
            [
              h(ProbeTagBlock, { candidate }),
              h(CandidateUnpricedTag, { candidate }),
              isCheapest
                ? h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => t('models.cheapestTag') })
                : null,
            ].filter(Boolean),
          ),
          h(
            'div',
            { onClick: (e: MouseEvent) => e.stopPropagation() },
            [h(NButton, { size: 'tiny', class: 'model-compare-edit', onClick: () => openCandidateEdit(row, candidate) }, { default: () => t('common.edit') })],
          ),
        ],
      )
    })],
  )
  return expandPanel({ eyebrow: isMobile.value ? undefined : t('models.expandComparison'), indent: !isMobile.value }, [list])
}

function onToggleStatus(row: Model, enable: boolean) {
  toggleStatusWithConfirm(
    enable,
    () => modelDisableCopy(row.id, t),
    async () => {
      try {
        await store.setStatus(row.id, enable)
        await reload()
      } catch (err) {
        message.error(displayMessage(err, t))
      }
    },
  )
}

// The model-level bulk selection column (desktop only): the header checkbox
// is the select-all over the CURRENT FILTERED models (all pages of them), and
// each row's checkbox joins or leaves that model — its click must not
// trigger the row's navigation.
const bulkSelectColumn = computed<DataTableColumns<Model>>(() => [
  {
    title: () =>
      h(NCheckbox, {
        size: 'small',
        class: 'models-select-all',
        checked: selectAllStatus.value === 'all',
        indeterminate: selectAllStatus.value === 'partial',
        'aria-label': t('models.bulkSelectAll'),
        'onUpdate:checked': onSelectAll,
      }),
    key: 'bulk_select',
    width: 48,
    render: (row) =>
      h('div', { onClick: (e: MouseEvent) => e.stopPropagation() }, [
        h(NCheckbox, {
          size: 'small',
          class: `models-row-check--${row.id}`,
          checked: selectedModelIds.value.has(row.id),
          'aria-label': row.name,
          'onUpdate:checked': () => onModelCheck(row.id),
        }),
      ]),
  },
])

// Desktop gets a real expand column (price comparison at a glance without leaving
// the list) behind the leading selection column; mobile card mode uses
// columns[0] as the card header, so neither the checkbox nor the expand
// column may lead there — the same panel becomes a labeled card row instead,
// and the selection layer renders nothing at all.
const columns = computed<DataTableColumns<Model>>(() =>
  isMobile.value
    ? [
        ...sharedColumns.value,
        {
          title: columnTitle(t('models.expandComparison'), t('models.expandComparison_tip')),
          key: 'expand_candidates',
          render: renderModelExpand,
        },
      ]
    : [...bulkSelectColumn.value, { type: 'expand', renderExpand: renderModelExpand }, ...sharedColumns.value],
)

// The price summary cell: the lowest input price among the model's PRICED
// token candidates (the shared predicate's verdict; an explicitly free row
// participates with 0), a warning tag counting the unpriced candidates
// (image and video rows always carry tier prices and never count), or the
// tag alone when no priced token candidate exists. The number formats exactly
// like the price lines' raw numbers, so the summary and the comparison rows
// can never disagree. Inline styles because scoped CSS does not reach
// h()-rendered nodes.
function renderPriceCell(row: Model) {
  const metrics = modelPriceMetrics(row.candidates)
  const parts: VNodeChild[] = []
  if (metrics.minTokenInputPrice !== null) {
    parts.push(
      h(
        'span',
        { class: 'model-price-min', style: 'font-weight:600; font-variant-numeric:tabular-nums;' },
        t('models.priceFrom', { price: metrics.minTokenInputPrice }),
      ),
    )
  }
  if (metrics.unpricedCount > 0) {
    parts.push(
      h(
        NTag,
        { size: 'small', type: 'warning', bordered: false, class: 'model-price-unpriced' },
        { default: () => `${t('models.unpricedCount')} ${metrics.unpricedCount}` },
      ),
    )
  }
  if (!parts.length) return h('span', { style: 'color:var(--color-text-muted);' }, '—')
  return h('div', { style: 'display:flex; align-items:center; gap:6px; flex-wrap:wrap;' }, parts)
}

const sharedColumns = computed<DataTableColumns<Model>>(() => [
  {
    title: columnTitle(t('models.name'), t('models.name_tip')),
    key: 'name',
    minWidth: 200,
    render: (row) => {
      // Text-only models carry no badge (the default modality would badge
      // every row); image-only and both models carry theirs beside the name.
      const badge = modalityBadge(row.output_modalities)
      const name = h('span', { class: 'model-name-cell' }, row.name)
      if (!badge) return name
      return h('div', { style: 'display: flex; align-items: center; gap: 6px;' }, [
        name,
        h(NTag, { size: 'small', bordered: false, type: 'info' }, { default: () => modalityBadgeLabel(badge, t) }),
      ])
    },
  },
  {
    title: columnTitle(t('models.runningStatusColumn'), t('models.runningStatusColumn_tip')),
    key: 'running_status',
    width: 180,
    render: (row) => {
      const display = modelRunningStatusDisplay(row.running_status)
      return h(NTag, { size: 'small', bordered: false, type: display.tagType }, { default: () => t(`models.running${display.i18nKey}`) })
    },
  },
  {
    title: columnTitle(t('models.candidateCountColumn'), t('models.candidateCountColumn_tip')),
    key: 'candidates',
    width: 140,
    render: (row) => `${row.candidates.filter((c) => c.routable).length} / ${row.candidates.length}`,
  },
  {
    title: columnTitle(t('models.firstRouteColumn'), t('models.firstRouteColumn_tip')),
    key: 'first_route',
    minWidth: 200,
    render: (row) => {
      // A balanced model has no fixed primary: each caller key enters the
      // chain at its own bound provider, so the cell names the mode instead
      // of candidates[0].
      const balanced = isBalancedModel(row)
      const first = row.candidates[0]
      const head = balanced
        ? h(NTag, { size: 'small', bordered: false }, { default: () => t('models.firstRouteBalanced') })
        : first
          ? `${first.provider_name} / ${first.provider_model_name}`
          : '-'
      // With a provider filter active, a row can match through a candidate
      // that is not the one shown here — the very thing that makes the row
      // look wrong ("why did Zhipu surface a deepseek model?"). Name the
      // matching candidate so the connection stays visible in both modes.
      const pid = applied.provider
      const matched = pid === null ? undefined : row.candidates.find((c) => c.provider_id === pid)
      if (!matched || (!balanced && matched === first)) return head
      return h('div', [
        h('div', [head]),
        h(
          'div',
          { style: 'font-size: 12px; color: #999;' },
          t('models.filterMatchedVia', {
            route: `${matched.provider_name} / ${matched.provider_model_name}`,
            position: matched.sort_order,
          }),
        ),
      ])
    },
  },
  {
    title: columnTitle(t('models.priceColumn'), t('models.priceColumn_tip')),
    key: 'price_summary',
    minWidth: 140,
    render: renderPriceCell,
  },
  {
    title: columnTitle(t('models.managementStatusColumn'), t('models.managementStatusColumn_tip')),
    key: 'management_status',
    width: 100,
    render: (row) =>
      h(
        'div',
        { onClick: (e: MouseEvent) => e.stopPropagation() },
        [
          h(NSwitch, {
            size: 'small',
            value: row.management_status === 1,
            'onUpdate:value': (v: boolean) => onToggleStatus(row, v),
          }),
        ],
      ),
  },
  {
    title: t('common.actions'),
    key: 'actions',
    align: 'center',
    width: 90,
    render: (row) =>
      h(
        'div',
        { onClick: (e: MouseEvent) => e.stopPropagation() },
        [
          h(
            ResponsiveDropdown,
            {
              trigger: 'click',
              placement: 'bottom-end',
              triggerText: t('common.actions'),
              height: 240,
              options: [
                { label: t('models.editModel'), key: 'edit' },
                { label: t('costs.detail.viewCost'), key: 'viewCost' },
                // A management-disabled model is not routable (the gateway
                // lists enabled models only), so no key is compatible with
                // it — the export entry would only land the picker in its
                // empty state, whose "create a key" guidance would mislead.
                ...(row.management_status === 1
                  ? [{ label: t('ccswitch.importAction'), key: 'importCCSImport' }]
                  : []),
                { type: 'divider', key: 'd' },
                { label: t('models.deleteModel'), key: 'delete', props: { style: 'color: var(--color-danger)' } },
              ],
              onSelect: (key: string) => {
                if (key === 'edit') openEditModel(row)
                else if (key === 'viewCost') router.push(modelCostDetailLocation(row.name))
                else if (key === 'importCCSImport') openCCSImport(row)
                else if (key === 'delete') deletingModel.value = row
              },
            },
            {
              default: () =>
                h(
                  NButton,
                  { size: 'small', quaternary: true, circle: true },
                  { icon: () => h(MoreHorizontal, { size: 16 }) },
                ),
            },
          ),
        ],
      ),
  },
])
</script>

<style scoped>
:deep(.model-name-cell) {
  font-weight: 650;
  color: var(--color-text);
}

.filter-switch {
  display: inline-flex;
  width: auto;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--color-text-secondary);
  cursor: pointer;
}

/* --- Bulk flow drawer (the pricing page's shape) --- */

.bulk-choose__field {
  margin-bottom: var(--space-4);
}

.bulk-choose__label {
  display: block;
  margin-bottom: var(--space-2);
  font-size: var(--text-sm);
  color: var(--color-text-secondary);
}

.bulk-choose__field .n-radio-group {
  display: inline-flex;
  flex-wrap: wrap;
  gap: var(--space-4);
}

.bulk-choose__hint {
  margin: var(--space-2) 0 0;
  font-size: var(--text-xs);
  color: var(--color-text-muted);
}

.bulk-factor {
  margin-top: var(--space-2);
  max-width: 200px;
}

.bulk-choose__error {
  margin: var(--space-2) 0 0;
  font-size: var(--text-xs);
  color: var(--color-danger);
}

.bulk-preview__summary {
  margin: 0 0 var(--space-3);
  font-size: var(--text-sm);
  color: var(--color-text-secondary);
}

.bulk-preview__none {
  margin: 0 0 var(--space-3);
  font-size: var(--text-sm);
  color: var(--color-danger);
}

.bulk-preview__item {
  padding: var(--space-2) var(--space-3);
  margin-bottom: var(--space-2);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
}

/* A skipped row greys out: it will not change, only explain itself. */
.bulk-preview__item--skipped {
  opacity: 0.55;
}

.bulk-preview__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
}

.bulk-preview__name {
  font-size: var(--text-sm);
  font-weight: 600;
}

.bulk-preview__detail {
  display: flex;
  flex-wrap: wrap;
  gap: 4px var(--space-3);
  margin-top: 4px;
  font-size: var(--text-xs);
  font-variant-numeric: tabular-nums;
  color: var(--color-text-secondary);
}

.bulk-preview__reason {
  color: var(--color-text-muted);
}
</style>
