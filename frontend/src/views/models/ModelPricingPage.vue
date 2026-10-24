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
     available; no selection UI exists on this page in any viewport. -->
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
             tooltip; the actions label stays a plain string. -->
        <div v-if="!isMobile" class="pricing-row-labels">
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
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage } from 'naive-ui'
import { ChevronDown, ChevronRight, Search, SearchX, Tags } from '@lucide/vue'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { CANDIDATE_STATUS_DISABLED, CANDIDATE_STATUS_ENABLED } from '../../api/candidateStatus'
import { useIsMobile } from '../../composables/useIsMobile'
import type { PricingFilter, PricingGroup, PricingRow } from './modelPricingGroups'
import { billingModeDisplay, buildPricingGroups, emptyPricingFilter, filterPricingGroups } from './modelPricingGroups'
import { PriceLinesBlock, ProbeTagBlock } from './pricingRowBlocks'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import CandidateEditModal from '../../components/models/CandidateEditModal.vue'
import CandidateUnpricedTag from '../../components/models/CandidateUnpricedTag.vue'

const { t } = useI18n()
const message = useMessage()
const store = useModelsStore()
const isMobile = useIsMobile()

// One full fetch per visit (plus manual refresh and post-edit reloads); the
// store's fetchId guard keeps a stale response from overwriting a newer one.
// Exposed as `reload` so follow-up flows (e.g. the bulk-apply toolbar) can
// refresh the list through the same guarded path.
async function reload(): Promise<void> {
  await store.fetchList()
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
  --pricing-grid-cols: minmax(120px, 1fr) minmax(150px, 1.2fr) minmax(150px, 1.3fr) 96px 130px 96px 180px;
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
</style>
