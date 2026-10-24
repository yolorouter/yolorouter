<!-- frontend/src/views/models/ModelPricingPage.vue
     The model pricing comparison page: GET /api/admin/models fetched once on
     mount (plus the manual refresh, plus a reload other flows can call),
     grouped client-side per model with candidates. Each group header carries
     the pricing summary — provider (mapping) count, the lowest input price
     among the group's PRICED token rows, the unpriced warning count, the
     model disabled badge — and each row shows the mapping's provider link,
     upstream name, shared price lines, billing mode, probe badge, unpriced
     badge, management state, and its edit / model-detail actions. The
     grouping rules, row sort, and the three-part client-side filter live in
     the pure modelPricingGroups module; this component only wires state
     (draft/applied filter pair, expand/collapse set, the shared edit modal)
     around it. Below the shared 768px breakpoint the rows render as stacked
     labeled cards — browse, filter, expand, and single-row edit stay
     available. Bulk pricing is a desktop capability: row checkboxes, a
     tri-state checkbox per group header, and a select-all scoped to the
     filtered visible rows feed a two-phase drawer (operation chooser, then
     the dry-run preview whose confirm applies) — none of that renders on
     mobile. The selection set algebra lives in the pure pricingBulkSelection
     module. -->
<template>
  <div class="common-page">
    <PageHeader
      :eyebrow="t('modelPricing.eyebrow')"
      :title="t('modelPricing.pageTitle')"
      :description="t('modelPricing.pageDescription')"
    >
      <template #actions>
        <n-button size="small" @click="expandAll">{{ t('modelPricing.expandAll') }}</n-button>
        <n-button size="small" @click="collapseAll">{{ t('modelPricing.collapseAll') }}</n-button>
        <n-button size="small" @click="onRefresh">{{ t('modelPricing.refresh') }}</n-button>
      </template>
    </PageHeader>

    <!-- Nothing fetched at all (or a total failure with an empty store): the
         shared empty state, the same component every list page uses. -->
    <EmptyState
      v-if="!store.loading && groups.length === 0"
      :icon="Tags"
      :title="t('modelPricing.emptyTitle')"
      :description="t('modelPricing.emptyDescription')"
    />
    <template v-else>
      <!-- Draft/applied pair over the fully-fetched list: the keyword edits a
           draft and applies on Search / Enter; the switch and the provider
           dropdown apply immediately, mirroring the models list page. -->
      <div class="filter-panel pricing-filter">
        <div class="filter-grid">
          <div class="filter-item filter-item--search">
            <n-input
              v-model:value="filter.keyword"
              :placeholder="t('modelPricing.filterKeyword')"
              clearable
              size="small"
              @keyup.enter="applyFilters"
            >
              <template #prefix><Search :size="14" /></template>
            </n-input>
          </div>
          <FilterSelectField
            v-model:value="filter.provider"
            :label="t('modelPricing.filterProviderLabel')"
            :options="providerOptions"
            :placeholder="t('modelPricing.filterAllProviders')"
            size="small"
            width="100%"
            @update:value="applyFilters"
          />
          <label class="filter-item pricing-filter__switch">
            <n-switch size="small" :value="filter.unpricedOnly" @update:value="onUnpricedOnlyChange" />
            <span>{{ t('modelPricing.filterUnpricedOnly') }}</span>
          </label>
          <div class="filter-actions">
            <!-- The bulk entry travels with the selection layer: desktop only,
                 disabled until at least one visible row is ticked. -->
            <n-button
              v-if="!isMobile"
              size="small"
              type="warning"
              ghost
              class="pricing-bulk-button"
              :disabled="selectedIds.size === 0"
              @click="openBulk"
            >
              <template #icon><BadgePercent :size="14" /></template>
              {{ bulkButtonLabel }}
            </n-button>
            <n-button size="small" type="primary" @click="applyFilters">{{ t('modelPricing.applyFilter') }}</n-button>
            <n-button size="small" quaternary @click="resetFilters">{{ t('modelPricing.resetFilter') }}</n-button>
          </div>
        </div>
      </div>

      <EmptyState
        v-if="!store.loading && visibleGroups.length === 0"
        :icon="SearchX"
        :title="t('modelPricing.filterEmptyTitle')"
        :description="t('modelPricing.filterEmptyDescription')"
      />
      <div v-else class="pricing-groups">
        <!-- Desktop grid labels: the data columns carry the shared help
             tooltip; the actions label stays a plain string. The leading
             checkbox is the select-all over the filtered visible rows. -->
        <div v-if="!isMobile" class="pricing-row-labels">
          <span class="pricing-row-label pricing-row-label--check">
            <n-checkbox
              class="pricing-select-all"
              size="small"
              :checked="selectAllStatus === 'all'"
              :indeterminate="selectAllStatus === 'partial'"
              :aria-label="t('modelPricing.bulkSelectAll')"
              @update:checked="onSelectAll"
            />
          </span>
          <span class="pricing-row-label">
            <HelpLabel :tip="t('modelPricing.colProvider_tip')">{{ t('modelPricing.colProvider') }}</HelpLabel>
          </span>
          <span class="pricing-row-label">
            <HelpLabel :tip="t('modelPricing.colProviderModelName_tip')">{{ t('modelPricing.colProviderModelName') }}</HelpLabel>
          </span>
          <span class="pricing-row-label">
            <HelpLabel :tip="t('modelPricing.colPrice_tip')">{{ t('modelPricing.colPrice') }}</HelpLabel>
          </span>
          <span class="pricing-row-label">
            <HelpLabel :tip="t('modelPricing.colBillingMode_tip')">{{ t('modelPricing.colBillingMode') }}</HelpLabel>
          </span>
          <span class="pricing-row-label">
            <HelpLabel :tip="t('modelPricing.colProbeState_tip')">{{ t('modelPricing.colProbeState') }}</HelpLabel>
          </span>
          <span class="pricing-row-label">
            <HelpLabel :tip="t('modelPricing.colManagement_tip')">{{ t('modelPricing.colManagement') }}</HelpLabel>
          </span>
          <span class="pricing-row-label">{{ t('common.actions') }}</span>
        </div>

        <section v-for="group in visibleGroups" :key="group.model.id" class="pricing-group">
          <header class="pricing-group__header" @click="toggleGroup(group.model.id)">
            <!-- The group's tri-state checkbox (desktop only): all / partial
                 / none over the group's VISIBLE rows; the click must not
                 toggle the group's expansion. -->
            <n-checkbox
              v-if="!isMobile"
              class="pricing-group-check"
              :class="'pricing-group-check--' + group.model.id"
              size="small"
              :checked="groupSelectionStatus(group.rows, selectedIds) === 'all'"
              :indeterminate="groupSelectionStatus(group.rows, selectedIds) === 'partial'"
              :aria-label="t('modelPricing.bulkSelectGroup')"
              @update:checked="(checked: boolean) => onGroupCheck(group, checked)"
              @click.stop
            />
            <button type="button" class="pricing-group__toggle" :aria-expanded="isExpanded(group.model.id)">
              <ChevronDown v-if="isExpanded(group.model.id)" :size="16" />
              <ChevronRight v-else :size="16" />
              <span class="pricing-group__name">{{ group.model.name }}</span>
            </button>
            <!-- The summary metrics sit beside the name; clicks here must not
                 toggle the group (the help tooltips are clickable too). -->
            <div class="pricing-group__metrics" @click.stop>
              <span class="pricing-group__metric pricing-group__providers">
                <HelpLabel :tip="t('modelPricing.providerCount_tip')">{{ t('modelPricing.providerCount') }}</HelpLabel>
                <span class="pricing-group__metric-value">{{ group.providerCount }}</span>
              </span>
              <span v-if="group.minTokenInputPrice !== null" class="pricing-group__metric pricing-group__min-price">
                <HelpLabel :tip="t('modelPricing.minInputPrice_tip')">{{ t('modelPricing.minInputPrice') }}</HelpLabel>
                <span class="pricing-group__metric-value">{{ group.minTokenInputPrice }}</span>
              </span>
              <n-tag v-if="group.unpricedCount > 0" size="small" type="warning" :bordered="false" class="pricing-group__unpriced">
                <HelpLabel :tip="t('modelPricing.unpricedCount_tip')">{{ t('modelPricing.unpricedCount') }}</HelpLabel>
                <span class="pricing-group__metric-value">{{ group.unpricedCount }}</span>
              </n-tag>
              <n-tag
                v-if="group.model.management_status !== CANDIDATE_STATUS_ENABLED"
                size="small"
                :bordered="false"
                class="pricing-group__model-disabled"
              >
                {{ t('modelPricing.modelDisabled') }}
              </n-tag>
            </div>
          </header>

          <div v-if="isExpanded(group.model.id)" class="pricing-group__body">
            <div
              v-for="row in group.rows"
              :key="row.candidate.id"
              class="pricing-row"
              :class="isMobile ? 'pricing-row--mobile' : 'pricing-row--desktop'"
            >
              <!-- The row's selection checkbox (desktop only); the label names
                   the row for screen readers. -->
              <div v-if="!isMobile" class="pricing-row__cell pricing-row__check">
                <n-checkbox
                  class="pricing-row-check"
                  :class="'pricing-row-check--' + row.candidate.id"
                  size="small"
                  :checked="selectedIds.has(row.candidate.id)"
                  :aria-label="row.candidate.provider_name + ' ' + (row.candidate.provider_model_name || row.model.name)"
                  @update:checked="() => onRowCheck(row.candidate.id)"
                />
              </div>
              <div class="pricing-row__cell">
                <span v-if="isMobile" class="pricing-row__field-label">{{ t('modelPricing.colProvider') }}</span>
                <router-link class="pricing-row__provider" :to="providerRoute(row)">{{ row.candidate.provider_name }}</router-link>
              </div>
              <div class="pricing-row__cell pricing-row__upstream">
                <span v-if="isMobile" class="pricing-row__field-label">{{ t('modelPricing.colProviderModelName') }}</span>
                <span class="pricing-row__field-value pricing-row__upstream-value">
                  <span class="pricing-row__upstream-name">{{ row.candidate.provider_model_name || '—' }}</span>
                  <!-- The unpriced badge, riding the name like the mapping
                       tables do: the shared single-source tag (badge,
                       tooltip, and the gating predicate live together). -->
                  <CandidateUnpricedTag :candidate="row.candidate" />
                </span>
              </div>
              <div class="pricing-row__cell pricing-row__price">
                <span v-if="isMobile" class="pricing-row__field-label">{{ t('modelPricing.colPrice') }}</span>
                <span class="pricing-row__field-value"><PriceLinesBlock :candidate="row.candidate" /></span>
              </div>
              <div class="pricing-row__cell">
                <span v-if="isMobile" class="pricing-row__field-label">{{ t('modelPricing.colBillingMode') }}</span>
                <span class="pricing-row__field-value">
                  <n-tag size="tiny" :bordered="false" :type="modeDisplay(row).tagType">{{ t(modeDisplay(row).labelKey) }}</n-tag>
                </span>
              </div>
              <div class="pricing-row__cell pricing-row__probe">
                <span v-if="isMobile" class="pricing-row__field-label">{{ t('modelPricing.colProbeState') }}</span>
                <span class="pricing-row__field-value"><ProbeTagBlock :candidate="row.candidate" /></span>
              </div>
              <div class="pricing-row__cell">
                <span v-if="isMobile" class="pricing-row__field-label">{{ t('modelPricing.colManagement') }}</span>
                <span class="pricing-row__field-value">
                  <n-tag v-if="row.candidate.management_status === CANDIDATE_STATUS_DISABLED" size="small" type="warning" :bordered="false">
                    {{ t('models.statusDisabled') }}
                  </n-tag>
                  <span v-else class="pricing-row__quiet">—</span>
                </span>
              </div>
              <div class="pricing-row__cell pricing-row__actions">
                <n-button size="tiny" @click="openEdit(row)">{{ t('modelPricing.editMapping') }}</n-button>
                <router-link class="pricing-row__model-link" :to="modelRoute(row)">{{ t('modelPricing.goToModel') }}</router-link>
              </div>
            </div>
          </div>
        </section>
      </div>
    </template>

    <!-- The shared candidate edit modal the mapping tables use: opened on a
         row with THAT row's model (the PATCH URL's owner) and the row itself;
         saved applies the same PATCH write path, then the list refreshes. -->
    <CandidateEditModal
      v-model:show="showEditCandidate"
      :model-id="editingRow?.model.id ?? 0"
      :model-name="editingRow?.model.name ?? ''"
      :editing-candidate="editingRow?.candidate ?? null"
      @saved="onCandidateEdited"
    />

    <!-- The desktop bulk-price flow, two phases in one drawer (the import
         modal's shape): the operation chooser, then the dry-run preview.
         Confirm on phase one previews (dry_run true); confirm on phase two
         applies (dry_run false). Cancel / mask / Esc close at any point
         without ever sending the write — except while a request is in
         flight: the dismissable lock (NewProviderModal's !submitting shape)
         withholds every dismiss affordance for the preview fetch and seals
         the apply write's point of no return against cancel / mask / Esc. -->
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
          <span class="bulk-choose__label">{{ t('modelPricing.bulkOperationLabel') }}</span>
          <n-radio-group v-model:value="bulkOperation" name="bulk-operation">
            <n-radio value="scale">{{ t('modelPricing.bulkOperationScale') }}</n-radio>
            <n-radio value="apply_suggested">{{ t('modelPricing.bulkOperationApplySuggested') }}</n-radio>
          </n-radio-group>
          <p class="bulk-choose__hint">
            {{ bulkOperation === 'scale' ? t('modelPricing.bulkOperationScale_tip') : t('modelPricing.bulkOperationApplySuggested_tip') }}
          </p>
        </div>
        <div v-if="bulkOperation === 'scale'" class="bulk-choose__field">
          <HelpLabel :tip="t('modelPricing.bulkFactorLabel_tip')">{{ t('modelPricing.bulkFactorLabel') }}</HelpLabel>
          <n-input-number
            v-model:value="bulkFactor"
            class="bulk-factor"
            :min="0"
            :step="0.01"
            :placeholder="t('modelPricing.bulkFactorPlaceholder')"
          />
          <p v-if="factorInvalid" class="bulk-choose__error">{{ t('modelPricing.bulkFactorInvalid') }}</p>
        </div>
      </div>
      <div v-else-if="bulkPreview" class="bulk-preview">
        <p class="bulk-preview__summary">
          {{ t('modelPricing.bulkPreviewSummary', { applied: bulkPreview.applied, skipped: bulkPreview.skipped }) }}
        </p>
        <p v-if="bulkPreview.applied === 0" class="bulk-preview__none">{{ t('modelPricing.bulkPreviewNothingToApply') }}</p>
        <div
          v-for="item in bulkPreview.items"
          :key="item.candidate_id"
          class="bulk-preview__item"
          :class="{ 'bulk-preview__item--skipped': !item.changed }"
        >
          <div class="bulk-preview__head">
            <span class="bulk-preview__name">{{ item.model_name }} · {{ item.provider_name }}</span>
            <n-tag size="tiny" :bordered="false" :type="item.changed ? 'success' : 'warning'">
              {{ item.changed ? t('modelPricing.bulkChangedTag') : t('modelPricing.bulkSkippedTag') }}
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
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
// NCheckbox / NRadio / NRadioGroup are NOT in main.ts's create() components
// list — without this import Vue renders them as unknown elements (typecheck
// and build stay green while the UI breaks).
import { NCheckbox, NRadio, NRadioGroup, useMessage } from 'naive-ui'
import { BadgePercent, ChevronDown, ChevronRight, Search, SearchX, Tags } from '@lucide/vue'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { CANDIDATE_STATUS_DISABLED, CANDIDATE_STATUS_ENABLED } from '../../api/candidateStatus'
import { useIsMobile } from '../../composables/useIsMobile'
import type { BulkPriceItem, BulkPriceOperation, BulkPricePayload, BulkPriceResult, ModelCandidate } from '../../api/models'
import { postBulkPrice } from '../../api/models'
import type { PricingFilter, PricingGroup, PricingRow } from './modelPricingGroups'
import { billingModeDisplay, buildPricingGroups, emptyPricingFilter, filterPricingGroups } from './modelPricingGroups'
import {
  groupSelectionStatus,
  previewSlotChanges,
  selectionStatus,
  toggleGroup as toggleGroupSelection,
  toggleRow,
  toggleSelectAll,
  visibleRowIds,
} from './pricingBulkSelection'
import { PriceLinesBlock, ProbeTagBlock } from './pricingRowBlocks'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import ModalDrawer from '../../components/common/ModalDrawer.vue'
import CandidateEditModal from '../../components/models/CandidateEditModal.vue'
import CandidateUnpricedTag from '../../components/models/CandidateUnpricedTag.vue'

const { t } = useI18n()
const message = useMessage()
const store = useModelsStore()
const isMobile = useIsMobile()

// One full fetch per visit (plus manual refresh and post-edit reloads); the
// store's fetchId guard keeps a stale response from overwriting a newer one.
// Exposed as `reload` so follow-up flows (the bulk-apply flow, the edit
// modal) can refresh the list through the same guarded path.
async function reload(): Promise<void> {
  await store.fetchList()
  // The selection names candidate ids; the fresh list may have reordered,
  // re-priced, or dropped them. A selection never outlives the refetch —
  // the next batch must be an explicit choice against current data.
  selectedIds.value = new Set()
}

function onRefresh() {
  void reload().catch((err) => message.error(displayMessage(err, t)))
}

onMounted(() => {
  void reload().catch((err) => message.error(displayMessage(err, t)))
})

// --- Grouping + filter state -----------------------------------------------

const groups = computed<PricingGroup[]>(() => buildPricingGroups(store.list))

const filter = reactive<PricingFilter>(emptyPricingFilter())
const applied = reactive<PricingFilter>(emptyPricingFilter())

function applyFilters() {
  Object.assign(applied, filter)
}

function resetFilters() {
  Object.assign(filter, emptyPricingFilter())
  Object.assign(applied, emptyPricingFilter())
}

function onUnpricedOnlyChange(value: boolean) {
  filter.unpricedOnly = value
  applyFilters()
}

const visibleGroups = computed<PricingGroup[]>(() => filterPricingGroups(groups.value, applied))

// Provider dropdown options: the distinct providers across the fetched data,
// so only providers that actually appear somewhere are offered.
const providerOptions = computed(() => {
  const byId = new Map<number, string>()
  for (const model of store.list) {
    for (const candidate of model.candidates) {
      if (!byId.has(candidate.provider_id)) byId.set(candidate.provider_id, candidate.provider_name)
    }
  }
  return [...byId.entries()]
    .map(([value, label]) => ({ label, value }))
    .sort((a, b) => a.label.localeCompare(b.label))
})

// A selected provider can drop out of the options when its last candidate
// goes away and the list refreshes — left in place it would be a ghost
// filter over an empty list.
watch(providerOptions, (options) => {
  const known = (value: number | null) => value === null || options.some((option) => option.value === value)
  if (!known(filter.provider)) filter.provider = null
  if (!known(applied.provider)) applied.provider = null
})

// --- Group expand / collapse ------------------------------------------------

const expandedIds = ref<Set<number>>(new Set())

function isExpanded(modelId: number): boolean {
  return expandedIds.value.has(modelId)
}

function toggleGroup(modelId: number) {
  const next = new Set(expandedIds.value)
  if (!next.delete(modelId)) next.add(modelId)
  expandedIds.value = next
}

function expandAll() {
  expandedIds.value = new Set(visibleGroups.value.map((group) => group.model.id))
}

function collapseAll() {
  expandedIds.value = new Set()
}

// --- Row helpers -------------------------------------------------------------

function modeDisplay(row: PricingRow) {
  return billingModeDisplay(row.candidate.billing_mode)
}

function providerRoute(row: PricingRow) {
  return `/providers/${row.candidate.provider_id}`
}

function modelRoute(row: PricingRow) {
  return `/models/${row.model.id}`
}

// --- Single-row edit (the shared modal, the shared PATCH write path) --------

const showEditCandidate = ref(false)
const editingRow = ref<PricingRow | null>(null)

function openEdit(row: PricingRow) {
  editingRow.value = row
  showEditCandidate.value = true
}

function onCandidateEdited() {
  void reload().catch((err) => message.error(displayMessage(err, t)))
}

// --- Bulk selection (desktop capability; mobile renders none of it) --------

// The ticked candidate ids across all groups. Pure set algebra lives in
// pricingBulkSelection; this block only wires the controls to it.
const selectedIds = ref<Set<number>>(new Set())

// Every fetched candidate by id: the preview's billing-mode lookup (the
// endpoint reports slot snapshots, not each row's billing mode).
const candidateById = computed<Map<number, ModelCandidate>>(() => {
  const byId = new Map<number, ModelCandidate>()
  for (const model of store.list) {
    for (const candidate of model.candidates) byId.set(candidate.id, candidate)
  }
  return byId
})

const bulkButtonLabel = computed(() =>
  selectedIds.value.size === 0
    ? t('modelPricing.bulkButton')
    : t('modelPricing.bulkButtonWithCount', { count: selectedIds.value.size }),
)

// The select-all reads the VISIBLE scope: the filtered rows, across groups.
const selectAllStatus = computed(() => selectionStatus(visibleRowIds(visibleGroups.value), selectedIds.value))

function onSelectAll(checked: boolean) {
  selectedIds.value = toggleSelectAll(visibleGroups.value, checked, selectedIds.value)
}

function onGroupCheck(group: PricingGroup, checked: boolean) {
  selectedIds.value = toggleGroupSelection(group.rows, checked, selectedIds.value)
}

function onRowCheck(candidateId: number) {
  selectedIds.value = toggleRow(candidateId, selectedIds.value)
}

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
// reloading against a drawer the user is no longer in. (Same pattern the
// dashboard and cost pages use for their reloads.)
let bulkReqSeq = 0

const factorInvalid = computed(() => bulkOperation.value === 'scale' && bulkFactor.value !== null && bulkFactor.value <= 0)

const bulkTitle = computed(() => (bulkPhase.value === 'choose' ? t('modelPricing.bulkDialogTitle') : t('modelPricing.bulkPreviewTitle')))

const bulkConfirmText = computed(() =>
  bulkPhase.value === 'choose' ? t('modelPricing.bulkPreviewButton') : t('modelPricing.bulkConfirmApply'),
)

// Choose phase: scale needs a positive factor, apply_suggested nothing.
// Preview phase: at least one row must be applicable for a write to mean
// anything (an all-skipped preview has nothing to confirm).
const bulkCanConfirm = computed(() => {
  if (bulkPhase.value === 'choose') {
    return bulkOperation.value === 'apply_suggested' || (bulkFactor.value !== null && bulkFactor.value > 0)
  }
  return (bulkPreview.value?.applied ?? 0) > 0
})

// The endpoint binds candidate_ids to 1..2000 raw: a selection past the cap
// would 400 no matter what the body says, so it is refused at the door with
// the numbers spelled out and the drawer stays closed.
const BULK_PRICE_MAX_IDS = 2000

function openBulk() {
  if (selectedIds.value.size === 0) return
  if (selectedIds.value.size > BULK_PRICE_MAX_IDS) {
    message.warning(t('modelPricing.bulkOverCap', { count: selectedIds.value.size, cap: BULK_PRICE_MAX_IDS }))
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
    candidate_ids: [...selectedIds.value],
    operation: bulkOperation.value,
    dry_run: dryRun,
  }
  if (bulkOperation.value === 'scale') payload.factor = bulkFactor.value!
  return payload
}

async function onBulkConfirm() {
  if (bulkBusy.value || !bulkCanConfirm.value || selectedIds.value.size === 0) return
  if (bulkPhase.value === 'choose') {
    bulkBusy.value = true
    const seq = ++bulkReqSeq
    try {
      const result = await postBulkPrice(bulkPayload(true))
      // The drawer can close — or close and reopen — while the dry run is in
      // flight. The token retires the response in both cases: a stale result
      // captures nothing, so it cannot flip a closed drawer into a preview
      // or skip the chooser on a reopened one (strictly stronger than the
      // is-the-drawer-still-open check it replaces).
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
    message.success(t('modelPricing.bulkAppliedSummary', { applied: result.applied, skipped: result.skipped }))
    // The write already happened: refresh and drop the selection even if the
    // refresh itself fails (its error is surfaced on its own).
    selectedIds.value = new Set()
    try {
      await reload()
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
  not_found: 'modelPricing.bulkSkipNotFound',
  unpriced: 'modelPricing.bulkSkipUnpriced',
  unsupported_billing_mode: 'modelPricing.bulkSkipUnsupportedBillingMode',
  no_suggestion: 'modelPricing.bulkSkipNoSuggestion',
  unchanged: 'modelPricing.bulkSkipUnchanged',
}

function skipReasonLabel(reason: BulkPriceItem['skip_reason']): string {
  return reason && SKIP_REASON_KEYS[reason] ? t(SKIP_REASON_KEYS[reason]) : t('modelPricing.bulkSkippedTag')
}

// A changed item's before→after entries, family per the row's billing mode
// (the endpoint reports slot snapshots; the page knows the mode).
function itemSlotChanges(item: BulkPriceItem) {
  return previewSlotChanges(candidateById.value.get(item.candidate_id)?.billing_mode, item.before, item.after)
}
</script>

<style scoped>
.pricing-filter {
  margin-bottom: var(--space-4);
}

.pricing-filter__switch {
  display: inline-flex;
  width: auto;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
  color: var(--color-text-secondary);
  cursor: pointer;
}

.pricing-groups {
  --pricing-grid-cols: 28px minmax(120px, 1fr) minmax(150px, 1.2fr) minmax(150px, 1.3fr) 96px 130px 96px 180px;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.pricing-group {
  background: var(--color-bg-elevated, var(--color-bg));
  border: 1px solid var(--color-border);
  border-radius: var(--radius-lg);
  overflow: hidden;
}

.pricing-group__header {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-3);
  padding: 10px var(--space-4);
  cursor: pointer;
}

.pricing-group__toggle {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 0;
  border: none;
  background: none;
  cursor: pointer;
  color: var(--color-text);
  font-size: var(--text-base);
  font-weight: 650;
}

.pricing-group__metrics {
  display: inline-flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--space-3);
  margin-left: auto;
}

.pricing-group__metric {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--text-sm);
  color: var(--color-text-secondary);
}

.pricing-group__metric-value {
  color: var(--color-text);
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.pricing-group__body {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-3) var(--space-4);
  border-top: 1px solid var(--color-border);
}

.pricing-row-labels {
  display: grid;
  grid-template-columns: var(--pricing-grid-cols);
  gap: var(--space-3);
  align-items: center;
  padding: 0 var(--space-4);
}

.pricing-row-label {
  font-size: var(--text-xs);
  color: var(--color-text-muted);
}

.pricing-row--desktop {
  display: grid;
  grid-template-columns: var(--pricing-grid-cols);
  gap: var(--space-3);
  align-items: center;
  padding: 8px var(--space-4);
  background: var(--color-bg);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
}

.pricing-row--mobile {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: var(--space-3);
  background: var(--color-bg);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);
}

.pricing-row__cell {
  min-width: 0;
}

/* On mobile the cells become label + value pairs; on desktop the labels
   never render, so the pair grid collapses onto its single child. */
.pricing-row--mobile .pricing-row__cell {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: var(--space-3);
  align-items: start;
}

.pricing-row__field-label {
  font-size: var(--text-xs);
  color: var(--color-text-muted);
  white-space: nowrap;
  padding-top: 2px;
}

.pricing-row__field-value {
  min-width: 0;
}

.pricing-row__upstream-value {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}

.pricing-row__provider,
.pricing-row__model-link {
  color: var(--color-accent);
}

.pricing-row__provider:hover,
.pricing-row__model-link:hover {
  text-decoration: underline;
}

.pricing-row__quiet {
  color: var(--color-text-muted);
}

.pricing-row__actions {
  display: inline-flex;
  align-items: center;
  gap: var(--space-3);
}

/* The selection column (desktop only): the checkbox centers itself in its
   narrow leading slot, both on the labels row and on data rows. */
.pricing-row-label--check,
.pricing-row__check {
  display: flex;
  align-items: center;
}

.pricing-group-check {
  flex-shrink: 0;
}

/* --- Bulk flow drawer --- */

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
