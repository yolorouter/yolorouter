<!-- frontend/src/views/models/ModelDetailPage.vue -->
<template>
  <div class="common-page" v-if="modelData">
    <PageHeader class="actions-placeholder" :eyebrow="t('models.eyebrow')" :title="modelData.name" :description="headerDescription">
      <!-- Same badge rule as the list page's name cell; absent for text-only models. -->
      <template v-if="headerModalityBadge" #title-suffix>
        <n-tag size="small" :bordered="false" type="info">
          {{ modalityBadgeLabel(headerModalityBadge, t) }}
        </n-tag>
      </template>
      <template #actions>
        <template v-if="!isMobile">
          <n-button size="small" @click="showEditModel = true">{{ t('models.editModel') }}</n-button>
          <n-button size="small" @click="router.push(modelCostDetailLocation(modelData.name))">
            {{ t('costs.detail.viewCost') }}
          </n-button>
          <n-button size="small" @click="onToggleModelStatus">
            {{ modelData.management_status === 1 ? t('models.statusDisabled') : t('models.statusEnabled') }}
          </n-button>
          <n-button size="small" type="error" ghost @click="showDeleteModel = true">
            {{ t('models.deleteModel') }}
          </n-button>
        </template>

        <ResponsiveDropdown
          v-else
          trigger="click"
          placement="bottom-end"
          :height="240"
          :trigger-text="t('common.actions')"
          :options="headerActionOptions"
          @select="onHeaderAction"
        />
      </template>
    </PageHeader>

    <!-- The admin-declared capability metadata (context length, modalities,
         tri-states, temperature) and the candidate max-output ceiling —
         read-only summary; editing lives in the edit modal. -->
    <ModelCapabilityCard :model="modelData" class="model-detail__capability-card" />

    <n-tabs v-model:value="activeTab" type="line" animated>
      <n-tab-pane name="route" :tab="t('models.tabRoute')">
        <div v-if="!isMobile" class="route-toolbar">
          <n-button @click="showAddCandidate = true">
            <template #icon><Plus :size="16" /></template>
            {{ t('models.addCandidate') }}
          </n-button>
        </div>

        <EmptyState v-if="modelData.candidates.length === 0" :title="t('models.routeChainEmpty')">
          <template #action>
            <n-button type="primary" @click="showAddCandidate = true">{{ t('models.addCandidate') }}</n-button>
          </template>
        </EmptyState>

        <!-- The shared mapping table: filter bar, columns, and row actions all
             live in CandidateTable; this page injects its row-identity
             adapter (its rows ARE the candidates of this page's one model),
             its own column groups, the capability tri-state columns, and the
             action handlers. -->
        <CandidateTable
          v-else
          :rows="candidateRows"
          :pagination="candidatePagination"
          :row-ids="candidateRowIds"
          :name-fields="candidateNameFields"
          :search-placeholder="`${t('models.provider')} / ${t('models.providerModelName')}`"
          :empty-title="t('models.routeChainEmpty')"
          :scroll-x="920"
          :leading-columns="candidateLeadingColumns"
          :price-column="candidatePriceColumn"
          :trailing-columns="candidateTrailingColumns"
          show-capability-columns
          :closing-columns="candidateClosingColumns"
          :busy-row-id="testingCandidateId"
          @edit="onEditCandidate"
          @retest="onCandidateRetest"
          @delete="onDeleteCandidate"
        />
      </n-tab-pane>

      <n-tab-pane name="impact" :tab="t('models.tabImpact')">
        <div class="section-card" style="white-space: pre-line">{{ impactText }}</div>
      </n-tab-pane>
    </n-tabs>

    <CandidateEditModal v-model:show="showAddCandidate" :model-id="modelData.id" :model-name="modelData.name" @saved="reload" />
    <CandidateEditModal
      v-model:show="showEditCandidate"
      :model-id="modelData.id"
      :model-name="modelData.name"
      :editing-candidate="editingCandidate"
      @saved="reload"
      @retest="onRetestCandidate"
    />
    <ModelEditModal v-model:show="showEditModel" :model="modelData" @updated="reload" />
    <DeleteModelModal v-model:show="showDeleteModel" :model="modelData" @deleted="onModelDeleted" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NSwitch, useDialog, useMessage, type DataTableColumns } from 'naive-ui'
import { ChevronDown, ChevronUp, Plus } from '@lucide/vue'
import { useModelsStore } from '../../store/models'
import { displayMessage } from '../../api/client'
import { useConfirmedStatusToggle } from '../../composables/useConfirmedStatusToggle'
import { modelDisableCopy, modelImpactOverview } from '../../utils/impactSummary'
import { candidateTestResultText, modelHeaderDescription, routableMark } from '../../utils/modelStatusDisplay'
import { redirectIfSessionExpired } from '../../utils/sessionExpiredRedirect'
import { isBalancedModel } from '../../utils/schedulingMode'
import { isTestSuccess } from '../../utils/testOutcomeDisplay'
import { modelCostDetailLocation } from '../../utils/modelCostLocation'
import { modalityBadge, modalityBadgeLabel } from '../../utils/modalityBadge'
import { getModelImpact, type Model, type ModelCandidate, type ModelImpact } from '../../api/models'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import CandidateTable, { type CandidatePriceColumn, type CandidateRowIds } from '../../components/models/CandidateTable.vue'
import CandidateEditModal from '../../components/models/CandidateEditModal.vue'
import ModelCapabilityCard from '../../components/models/ModelCapabilityCard.vue'
import ModelEditModal from '../../components/models/ModelEditModal.vue'
import DeleteModelModal from '../../components/models/DeleteModelModal.vue'
import ResponsiveDropdown from '../../components/common/ResponsiveDropdown.vue'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { useClientPagination } from '../../composables/useClientPagination'
import { useSingleRowAction } from '../../composables/useSingleRowAction'
import { useIsMobile } from '../../composables/useIsMobile.ts'

const { t, te } = useI18n()
const route = useRoute()
const router = useRouter()
const dialog = useDialog()
const toggleStatusWithConfirm = useConfirmedStatusToggle(dialog)
const message = useMessage()
const store = useModelsStore()
const isMobile = useIsMobile()

const modelId = Number(route.params.id)
const modelData = ref<Model | null>(null)
const activeTab = ref('route')
const showAddCandidate = ref(false)
const showEditCandidate = ref(false)
const showEditModel = ref(false)
const showDeleteModel = ref(false)

// The page's subject no longer exists after a delete — leave for the list
// (its onMounted fetch shows the table without the deleted row).
function onModelDeleted() {
  void router.push('/models')
}

// Client-side pagination for the route-chain candidate table — a single
// model's candidate list is short, so slice in-page rather than paging
// server-side.
const {
  pagination: candidatePagination,
} = useClientPagination()
const editingCandidate = ref<ModelCandidate | null>(null)

// The route-chain rows the shared table filters and pages. Search +
// verification-result filtering lives inside CandidateTable (it wires the
// shared composable itself); this page only names the two columns its keyword
// searches, and resolves row identities for the shared table's actions.
const candidateRows = computed(() => modelData.value?.candidates ?? [])
function candidateNameFields(row: ModelCandidate): string[] {
  return [row.provider_name, row.provider_model_name]
}

// The row-identity adapter for the shared table: this page's rows ARE the
// candidates of the page's one model, so the candidate id is the row's own id
// and the model id is the page's subject.
function candidateRowIds(row: ModelCandidate): CandidateRowIds {
  return { candidateId: row.id, modelId }
}

// The shared table's retest emit carries (row, ids) — from the '⋯' menu and
// from an unconfirmed capability tag alike; this page's retest runner speaks
// the bare candidate id.
function onCandidateRetest(_row: ModelCandidate, ids: CandidateRowIds) {
  onRetestCandidate(ids.candidateId)
}
// Tracks the single candidate currently running its own capability test so
// the actions button can show a spinner instead of silently doing nothing
// until the request resolves (mirrors ProviderDetailPage.vue's testingKeyId).
const testingCandidateId = ref<number | null>(null)
const reorderAction = useSingleRowAction()

// Balanced scheduling changes what this page's two scheduling-specific pieces
// mean: the binding-count column exists only for it, and the reorder column's
// hint must not say "priority" when order no longer decides who takes the
// traffic.
const isBalanced = computed(() => (modelData.value ? isBalancedModel(modelData.value) : false))

const headerDescription = computed(() =>
  modelHeaderDescription(t, modelData.value?.running_status ?? 'not_configured', isBalanced.value),
)

// Computed once: the template renders the badge (and its label) off this,
// never re-deriving the rule inline. Same rule as the list page's name cell;
// absent for text-only models.
const headerModalityBadge = computed(() => (modelData.value ? modalityBadge(modelData.value.output_modalities) : null))

// On mobile the header buttons collapse into a single ResponsiveDropdown, so the
// toggle-status row's label follows the model's current management_status.
const headerActionOptions = computed(() => [
  { label: t('models.editModel'), key: 'edit' },
  { label: t('costs.detail.viewCost'), key: 'viewCost' },
  { label: t('models.addCandidate'), key: 'addCandidate' },
  {
    label: modelData.value?.management_status === 1 ? t('models.statusDisabled') : t('models.statusEnabled'),
    key: 'toggleStatus',
  },
  { label: t('models.deleteModel'), key: 'delete', props: { style: 'color: var(--color-danger)' } },
])

function onHeaderAction(key: string) {
  if (key === 'edit') showEditModel.value = true
  else if (key === 'viewCost') router.push(modelCostDetailLocation(modelData.value!.name))
  else if (key === 'addCandidate') showAddCandidate.value = true
  else if (key === 'toggleStatus') onToggleModelStatus()
  else if (key === 'delete') showDeleteModel.value = true
}

onMounted(() => {
  void reload().catch((err) => message.error(displayMessage(err, t)))
})

async function reload() {
  modelData.value = await store.fetchDetail(modelId)
  // Refreshed alongside the detail so the impact tab tracks status changes;
  // a failed fetch degrades the tab, never the page.
  try {
    impact.value = await getModelImpact(modelId)
    impactFailed.value = false
  } catch {
    impact.value = null
    impactFailed.value = true
  }
}

const impact = ref<ModelImpact | null>(null)
const impactFailed = ref(false)
const impactText = computed(() => {
  if (impact.value) return modelImpactOverview(t, impact.value, true)
  return impactFailed.value ? t('models.impactLoadFailed') : t('common.loading')
})

function onEditCandidate(candidate: ModelCandidate) {
  editingCandidate.value = candidate
  showEditCandidate.value = true
}

async function onRetestCandidate(candidateId: number) {
  testingCandidateId.value = candidateId
  try {
    const { candidate: updated, applied } = await store.retestCandidate(modelId, candidateId)
    await reload()
    if (!applied) {
      // A concurrent probe won the commit race, so the row now carries ITS
      // result; announcing it as this click's could state the opposite of
      // what this run observed.
      message.info(t('providers.retestSuperseded'))
      return
    }
    // Two-tier feedback so the click is never silent: pass (green) vs. the
    // specific outcome reason (yellow).
    //
    // Judged on last_test_result — THIS run's basic-probe outcome — not on
    // verification_status. An inconclusive run (rate limited, unreachable)
    // deliberately leaves a previously passing status alone, so reading the
    // status would report "test passed" for a run the provider actually refused.
    const passed = updated.last_test_result !== null && isTestSuccess(updated.last_test_result)
    message[passed ? 'success' : 'warning'](candidateTestResultText(t, passed, updated.last_test_result))
  } catch (err) {
    if (redirectIfSessionExpired(err, router)) return
    message.error(displayMessage(err, t))
  } finally {
    testingCandidateId.value = null
  }
}

async function onReorder(candidateId: number, direction: 'up' | 'down') {
  await reorderAction.run(candidateId, async () => {
    try {
      await store.reorderCandidate(modelId, candidateId, direction)
      await reload()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  }, direction)
}

function onToggleCandidateStatus(candidateId: number, enable: boolean) {
  void (async () => {
    try {
      await store.setCandidateStatus(modelId, candidateId, enable)
      await reload()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  })()
}

function onDeleteCandidate(candidate: ModelCandidate) {
  const remainingRoutable = modelData.value!.candidates.filter((c) => c.routable && c.id !== candidate.id).length
  dialog.warning({
    title: t('models.confirmDeleteCandidateTitle'),
    content: candidate.sort_order === 1
      ? t('models.confirmDeleteFirstCandidateContent')
      : t('models.confirmDeleteCandidateContent', { count: remainingRoutable }),
    positiveText: t('models.save'),
    negativeText: t('models.cancel'),
    onPositiveClick: async () => {
      try {
        await store.deleteCandidate(modelId, candidate.id)
        await reload()
      } catch (err) {
        message.error(displayMessage(err, t))
      }
    },
  })
}

function onToggleModelStatus() {
  if (!modelData.value) return
  const enabling = modelData.value.management_status !== 1
  toggleStatusWithConfirm(
    enabling,
    () => modelDisableCopy(modelId, t),
    async () => {
      try {
        await store.setStatus(modelId, enabling)
        await reload()
      } catch (err) {
        message.error(displayMessage(err, t))
      }
    },
  )
}

// The routable mark is this page's own column (the provider page's mapping
// rows carry no routable flag).
function renderRoutable(row: ModelCandidate) {
  return routableMark(t, te, row)
}

// ---- The route tab's shared-table injections ----

// The price column leads its lines with the billing-mode tag — this table
// carries no other billing context, so the mode travels with the number it
// explains.
const candidatePriceColumn = computed<CandidatePriceColumn>(() => ({
  title: t('models.billingPriceColumn'),
  tip: t('models.billingPriceColumn_tip'),
  minWidth: 150,
  showBillingModeTag: true,
}))

// This page's own columns around the shared table's core: the provider name
// before it, then the management switch / binding gauge / routable mark after
// the price column (the capability tri-state columns render inside the shared
// table — these rows carry the capability flags), and the reorder buttons
// last before the shared actions column.
const candidateLeadingColumns = computed<DataTableColumns<ModelCandidate>>(() => [
  { title: columnTitle(t('models.provider'), t('models.provider_tip')), key: 'provider_name', minWidth: 140 },
])

const candidateTrailingColumns = computed<DataTableColumns<ModelCandidate>>(() => {
  // The binding count is the balanced mode's evenness gauge; a failover
  // model has no bindings, and a zero column there would read as "balanced
  // but nobody bound" — a state failover cannot even be in. Also hidden
  // below two view-routable candidates: with at most one live route the
  // relay binds nothing, so the column would show a permanent, misleading 0.
  // View routability is stricter than the relay's own eligibility (it also
  // requires a usable provider key), so the column can hide while the relay
  // still balances — erring toward hiding the gauge, never showing a bogus
  // one.
  const routableCount = modelData.value?.candidates.filter((c) => c.routable).length ?? 0
  const showBindingCount = isBalanced.value && routableCount >= 2
  const bindingCountColumn: DataTableColumns<ModelCandidate>[number] = {
    title: columnTitle(t('models.bindingCountColumn'), t('models.bindingCountColumn_tip')),
    key: 'binding_count',
    width: STATUS_COL_WIDTH,
    align: 'center',
    render: (row) => h('span', { style: 'font-variant-numeric: tabular-nums' }, String(row.binding_count)),
  }
  return [
    {
      title: columnTitle(t('models.managementStatusColumn'), t('models.managementStatusColumn_tip')),
      key: 'management_status',
      width: STATUS_COL_WIDTH,
      align: 'center',
      render: (row) => h(NSwitch, { value: row.management_status === 1, 'onUpdate:value': (v: boolean) => onToggleCandidateStatus(row.id, v) }),
    },
    ...(showBindingCount ? [bindingCountColumn] : []),
    {
      title: columnTitle(t('models.routableColumn'), t('models.routableColumn_tip')),
      key: 'routable',
      width: STATUS_COL_WIDTH,
      align: 'center',
      render: (row) => renderRoutable(row),
    },
  ]
})

const candidateClosingColumns = computed<DataTableColumns<ModelCandidate>>(() => [
  {
    // The column's meaning follows the model's scheduling mode: under
    // failover it is the priority list (the head takes all traffic), under
    // balanced it only fixes the rotation order and the failover path —
    // the same hint the mode's own tooltip explains, kept here so the
    // reorder buttons never promise priority they no longer deliver.
    // Button columns normally take a plain string title; this one
    // deliberately carries a header tip because its meaning flips with
    // the mode and the buttons themselves stay generic up/down.
    title: columnTitle(
      isBalanced.value ? t('models.reorderColumnBalanced') : t('models.reorderColumn'),
      isBalanced.value ? t('models.reorderColumnBalanced_tip') : t('models.reorderColumn_tip'),
    ),
    key: 'reorder',
    width: 70,
    align: 'center',
    render: (row, index) => {
      const count = modelData.value?.candidates.length ?? 0
      const r = reorderAction.activeId.value
      const reordering = r !== null
      const upLoading = r === row.id && reorderAction.direction.value === 'up'
      const downLoading = r === row.id && reorderAction.direction.value === 'down'
      return h('div', { style: 'display:inline-flex;align-items:center;gap:2px;justify-content:center' }, [
        h(
          NButton,
          { size: 'small', quaternary: true, circle: true, disabled: reordering || index === 0, loading: upLoading, title: t('models.moveUp'), onClick: () => onReorder(row.id, 'up') },
          { icon: () => h(ChevronUp, { size: 16 }) },
        ),
        h(
          NButton,
          { size: 'small', quaternary: true, circle: true, disabled: reordering || index >= count - 1, loading: downLoading, title: t('models.moveDown'), onClick: () => onReorder(row.id, 'down') },
          { icon: () => h(ChevronDown, { size: 16 }) },
        ),
      ])
    },
  },
])
</script>

<style scoped>
.route-toolbar {
  display: flex;
  justify-content: flex-end;
  margin-bottom: var(--space-4);
}

.model-detail__capability-card {
  margin-bottom: var(--space-6);
}
</style>
