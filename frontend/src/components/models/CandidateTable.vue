<!-- frontend/src/components/models/CandidateTable.vue
     The mapping (candidate) table shared by the provider detail page's models
     tab and the model detail page's route tab — one implementation, so the two
     views cannot drift apart again (they once did: the provider page's table
     lacked the edit/delete actions the model page had).

     The component owns what the two pages previously kept as duplicated code:
     the search + verification-result filter bar (wired onto the shared
     useCandidateTableFilter composable — the filtering logic layer is reused,
     not re-derived), the client-paged responsive table shell with its
     filter-miss empty state, the provider-model-name and price columns, and
     the '⋯' inline actions menu. The probe six-state badge column and the
     capability tri-state columns are config-gated: rendered only for the page
     whose rows carry the fields they read.

     Page differences arrive as data, never as branches on who is asking: each
     page injects a row-identity adapter (which row fields name the candidate
     and its model — the provider page's rows speak candidate_id/model_id, the
     model page's rows are the candidates themselves), its own column groups
     around the shared core, and the action handlers. The edit / retest /
     delete emits carry the row plus its resolved ids. The component never
     learns which page is rendering it. -->
<template>
  <div>
    <!-- Search + verification-result filter over the fully-fetched mapping
         list: an in-memory computed, so it always sees every row, not just
         the current page's slice. Any change restarts the pager at page 1
         (a narrowed list can strand the current page past the end). -->
    <div class="filter-panel candidate-filter">
      <div class="filter-grid">
        <div class="filter-item filter-item--search">
          <NInput v-model:value="keyword" :placeholder="searchPlaceholder" clearable size="small">
            <template #prefix><Search :size="14" /></template>
          </NInput>
        </div>
        <FilterSelectField
          v-model:value="bucket"
          :label="t('models.probeResultTitle')"
          :options="verificationFilterOptions"
          :placeholder="t('providers.verificationFilterAll')"
          size="small"
        />
      </div>
    </div>
    <div class="data-table-wrapper">
      <ResponsiveDataTable
        :checked-row-keys="checkedRowKeys"
        :columns="columns"
        :data="filteredRows"
        :loading="loading"
        :scroll-x="scrollX"
        :row-key="rowKey"
        :pagination="pagination"
        @update:checked-row-keys="onCheckedKeys"
      >
        <!-- A filter the whole list fails to match renders the tab's own
             empty state rather than a blank table body. -->
        <template #empty>
          <EmptyState :title="emptyTitle" />
        </template>
      </ResponsiveDataTable>
    </div>
  </div>
</template>

<script lang="ts">
// Module-scope type exports (a plain script block beside <script setup> is
// the SFC way to export types): the pages and tests import these to type
// their adapters and emit handlers against the component's contract.
import type { PricedCandidate } from '../../utils/candidatePriceLines'

/** The fields every mapping-table row carries — the ones the columns and the
 *  filter this component renders itself read. Page-injected column families
 *  may read more: the probe badge column needs the provider page's
 *  probe-queue fields, the capability columns need the model page's
 *  capability flags. */
export type CandidateTableRow = {
  verification_status: number
  provider_model_name: string
} & PricedCandidate

/** The two identifiers that name a mapping row everywhere per-row actions
 *  speak it: the candidate id (the row key, and the id single-flight spinners
 *  match against) and the model the mapping belongs to (the id the candidate
 *  PATCH/DELETE/DELETE URLs need). The two pages resolve these from
 *  different row shapes through their injected adapter. */
export interface CandidateRowIds {
  candidateId: number
  modelId: number
}

/** The price column's per-page shape. Both pages render the same shared price
 *  lines; the model page additionally leads them with the billing-mode tag
 *  (its table carries no other billing context) and uses its own titles. */
export interface CandidatePriceColumn {
  title: string
  tip: string
  minWidth?: number
  showBillingModeTag?: boolean
}
</script>

<script setup lang="ts" generic="Row extends CandidateTableRow">
import { computed, h } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NInput, NSpace, NTag, NTooltip, type DataTableColumns } from 'naive-ui'
import { MoreHorizontal, Search } from '@lucide/vue'
import EmptyState from '../EmptyState.vue'
import FilterSelectField from '../common/FilterSelectField.vue'
import ResponsiveDataTable from '../common/ResponsiveDataTable.vue'
import ResponsiveDropdown from '../common/ResponsiveDropdown.vue'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { candidatePriceLines } from '../../utils/candidatePriceLines'
import { candidateUnpriced } from '../../utils/imagePriceSummary'
import { renderProbeStateTag } from '../../utils/probeStateTag'
import { capabilityState } from '../../utils/modelStatusDisplay'
import type { BillingMode } from '../../api/models'
import { candidateVerificationFilterOptions, useCandidateTableFilter } from '../../composables/useCandidateTableFilter'
import type { ClientPagination } from '../../composables/useClientPagination'

// The row shapes the two config-gated column families read. TypeScript cannot
// tie "this config flag is on" to "Row carries these fields", so the gated
// renders narrow with these casts — the flags exist precisely because only
// one page's rows satisfy the shape.
type ProbeStateSource = Parameters<typeof renderProbeStateTag>[1]
type CapabilitySource = { supports_streaming: boolean | null; supports_function_calling: boolean | null }

const props = withDefaults(
  defineProps<{
    /** The FULL mapping list. Both tables are fully fetched and paged
     *  client-side; the filter runs over the whole list inside a computed,
     *  never the current page's slice. */
    rows: Row[]
    /** Client-side pagination object (useClientPagination). Filter changes
     *  restart it at page 1; the pager itself turns pages without refetching. */
    pagination: ClientPagination
    /** Row-identity adapter: resolves the candidate id + model id of a row.
     *  Feeds the row key, the busy-row matching, and the action emits. */
    rowIds: (row: Row) => CandidateRowIds
    /** The two name columns the keyword searches (each page passes the two
     *  name columns its table shows). The function is captured once at
     *  mount by the shared filter composable — pass a stable reference (a
     *  named function), not a fresh inline arrow. */
    nameFields: (row: Row) => string[]
    /** The search input's placeholder — each page composes it from its own
     *  two searchable columns' existing title terms. */
    searchPlaceholder: string
    /** Copy shown when a filter matches nothing (each tab's own "empty"
     *  wording, riding the table's empty slot in both layouts). */
    emptyTitle: string
    /** The price column's shape (titles and mode tag per page). */
    priceColumn: CandidatePriceColumn
    /** Table loading state (the provider page polls in the background;
     *  silent polls pass false so the table does not blink). */
    loading?: boolean
    /** Horizontal scroll width of the assembled columns. */
    scrollX?: number
    /** Page columns rendered FIRST — before the shared provider-model-name
     *  column. Provider page: the batch-selection column + the model name;
     *  model page: the provider name. */
    leadingColumns?: DataTableColumns<Row>
    /** Render the probe six-state badge column. Provider page only: its rows
     *  carry the probe-queue fields the badge state reads. */
    showProbeStateColumn?: boolean
    /** Page columns rendered after the price column (and after the probe
     *  badge column when that is shown). Provider page: the failure reason. */
    trailingColumns?: DataTableColumns<Row>
    /** Render the capability tri-state columns. Model page only: its rows
     *  carry the capability flags. */
    showCapabilityColumns?: boolean
    /** Page columns rendered last, right before the actions column (after
     *  the capability columns when those are shown). Model page: reorder. */
    closingColumns?: DataTableColumns<Row>
    /** The row whose per-row action is currently in flight: its '⋯' trigger
     *  spins and disables. null when nothing is running. */
    busyRowId?: number | null
    /** Whether a row gets the retest entry. The provider page shows it only
     *  to rows without a passing verdict that the probe queue does not
     *  already hold; the model page offers it on every row (default). */
    retestable?: (row: Row) => boolean
    /** Disable the retest entry while ANY row action is in flight — the
     *  provider page's single-flight rule (a second click would be silently
     *  swallowed). Off by default. */
    disableRetestWhileBusy?: boolean
    /** Selection pass-through for tables whose leading columns carry a
     *  `type: 'selection'` entry; the parent stays the owner of the checked
     *  keys, exactly as with a bare responsive table. */
    checkedRowKeys?: Array<string | number>
  }>(),
  {
    loading: false,
    scrollX: undefined,
    leadingColumns: undefined,
    showProbeStateColumn: false,
    trailingColumns: undefined,
    showCapabilityColumns: false,
    closingColumns: undefined,
    busyRowId: null,
    retestable: undefined,
    disableRetestWhileBusy: false,
    checkedRowKeys: undefined,
  },
)

const emit = defineEmits<{
  /** A row's edit entry was picked. The page owns the edit modal. */
  edit: [row: Row, ids: CandidateRowIds]
  /** A row's retest entry was picked — from the menu or by clicking an
   *  unconfirmed capability tag. The page owns the call and its feedback. */
  retest: [row: Row, ids: CandidateRowIds]
  /** A row's delete entry was picked. The page owns the confirm dialog and
   *  its last-routable-candidate guard. */
  delete: [row: Row, ids: CandidateRowIds]
  'update:checkedRowKeys': [keys: Array<string | number>]
}>()

const { t } = useI18n()

// Search + verification-result filter — the shared composable, so both pages'
// filtering behaves identically by construction (keyword over the two name
// columns, verification bucket over the raw three-value verification_status,
// any change restarting the pager at page 1).
const verificationFilterOptions = computed(() => candidateVerificationFilterOptions(t))
const { keyword, bucket, filteredRows } = useCandidateTableFilter(
  computed(() => props.rows),
  props.nameFields,
  props.pagination,
)

function rowKey(row: Row): string | number {
  return props.rowIds(row).candidateId
}

function onCheckedKeys(keys: Array<string | number>) {
  emit('update:checkedRowKeys', keys)
}

// The mode tag the model page's price column leads with: label and tag color
// keyed to the same billing-mode vocabulary, one table instead of two parallel
// ternary cascades drifting apart. Image and video carry their own tier
// tables; token reads the per-million slots — colors keep the three distinct
// at a glance.
function billingModeTag(mode: BillingMode): { labelKey: string; tagType: 'info' | 'success' | 'default' } {
  if (mode === 'image') return { labelKey: 'models.billingPerImage', tagType: 'info' }
  if (mode === 'video') return { labelKey: 'models.billingPerVideo', tagType: 'success' }
  if (mode === 'audio') return { labelKey: 'models.billingModeAudio', tagType: 'info' }
  return { labelKey: 'models.billingPerToken', tagType: 'default' }
}

// Capability cells render each state distinctly. An unconfirmed capability is
// not a failure — routing ignores these flags entirely — it just means no
// probe has confirmed it yet, so the tag is clickable to retest rather than
// alarming.
function renderCapability(row: Row, ids: CandidateRowIds, flag: boolean | null) {
  const busy = props.busyRowId === ids.candidateId
  switch (capabilityState(flag)) {
    case 'confirmed':
      return h(NTag, { size: 'small', type: 'success', bordered: false }, { default: () => '✓' })
    case 'unsupported':
      return h(NTag, { size: 'small', type: 'error', bordered: false }, { default: () => '✗' })
    default:
      return h(
        NTooltip,
        { trigger: 'hover' },
        {
          trigger: () =>
            h(
              NTag,
              {
                size: 'small',
                type: 'warning',
                bordered: false,
                style: busy ? 'cursor: progress' : 'cursor: pointer',
                // Clickable, so button semantics rather than the display-only
                // hintTag shape: focusable, named by the hint, and operable
                // from the keyboard.
                role: 'button',
                'aria-label': t('models.probeUnconfirmedHint'),
                tabindex: 0,
                onClick: () => {
                  if (!busy) emit('retest', row, ids)
                },
                onKeydown: (e: KeyboardEvent) => {
                  if ((e.key === 'Enter' || e.key === ' ') && !busy) {
                    e.preventDefault()
                    emit('retest', row, ids)
                  }
                },
              },
              { default: () => '?' },
            ),
          default: () => t('models.probeUnconfirmedHint'),
        },
      )
  }
}

const columns = computed<DataTableColumns<Row>>(() => {
  const cols: DataTableColumns<Row> = [...(props.leadingColumns ?? [])]

  cols.push({
    title: columnTitle(t('models.providerModelName'), t('models.providerModelName_tip')),
    key: 'provider_model_name',
    minWidth: 160,
    // The name carries the unpriced badge (token/audio rows the server flagged
    // undeclared — see candidateUnpriced): informational, with the pricing
    // guidance on its tooltip, so the operator knows the way out is the edit
    // dialog's pricing save. One renderer here rather than per page, so the
    // two views cannot drift apart again.
    render: (row) => {
      const parts = [h('span', row.provider_model_name || '-')]
      if (candidateUnpriced(row)) {
        parts.push(
          h(
            NTooltip,
            { trigger: 'hover' },
            {
              trigger: () =>
                h(NTag, { size: 'small', bordered: false, type: 'warning' }, { default: () => t('providers.candidateUnpriced') }),
              default: () => t('providers.candidateUnpriced_tip'),
            },
          ),
        )
      }
      return h(NSpace, { size: 6, align: 'center', wrapItem: false }, { default: () => parts })
    },
  })

  const price = props.priceColumn
  cols.push({
    // Billing + price in one column, same lines on both pages (the shared
    // candidate price renderer): the billing mode decides which price slot
    // means anything, so the model page's mode tag travels with the number it
    // explains. What the numbers mean lives in the header tooltip, keeping
    // the header itself to one word.
    title: columnTitle(price.title, price.tip),
    key: 'price',
    minWidth: price.minWidth ?? 120,
    render: (row) => {
      if (!price.showBillingModeTag) return h('div', candidatePriceLines(row, t))
      const tag = billingModeTag(row.billing_mode)
      const modeTag = h(NTag, { size: 'tiny', bordered: false, type: tag.tagType }, { default: () => t(tag.labelKey) })
      return h('div', { style: 'display:flex;flex-direction:column;align-items:flex-start;gap:2px;' }, [
        modeTag,
        ...candidatePriceLines(row, t),
      ])
    },
  })

  if (props.showProbeStateColumn) {
    cols.push({
      // The probe six-state badge: passed / passed-disabled / failed /
      // inconclusive / idle / pending (the queue's live position, or the
      // caller's untested fallback). One renderer shared with the import
      // progress view so the vocabulary can never drift.
      title: columnTitle(t('providers.candidateProbeStatus'), t('providers.candidateProbeStatus_tip')),
      key: 'probe_state',
      width: STATUS_COL_WIDTH,
      render: (row) =>
        renderProbeStateTag(t, row as unknown as ProbeStateSource, { labelKey: 'providers.verificationUntested', type: 'default' }),
    })
  }

  cols.push(...(props.trailingColumns ?? []))

  if (props.showCapabilityColumns) {
    cols.push(
      {
        title: columnTitle(t('models.supportsStreaming'), t('models.supportsStreaming_tip')),
        key: 'supports_streaming',
        width: STATUS_COL_WIDTH,
        align: 'center',
        render: (row) => renderCapability(row, props.rowIds(row), (row as unknown as CapabilitySource).supports_streaming),
      },
      {
        title: columnTitle(t('models.supportsFunctionCalling'), t('models.supportsFunctionCalling_tip')),
        key: 'supports_function_calling',
        width: STATUS_COL_WIDTH,
        align: 'center',
        render: (row) => renderCapability(row, props.rowIds(row), (row as unknown as CapabilitySource).supports_function_calling),
      },
    )
  }

  cols.push(...(props.closingColumns ?? []))

  cols.push({
    // Actions-column convention (both pages): one compact "⋯" dropdown
    // instead of inline text buttons. Edit/delete apply to EVERY row — a
    // passed mapping is exactly the one worth repricing or renaming. The
    // retest entry's presence is the retestable predicate's call; on the
    // provider page rows the probe queue holds get no retest because their
    // probe is already coming.
    title: t('common.actions'),
    key: 'actions',
    width: 60,
    align: 'center',
    render: (row) => {
      const ids = props.rowIds(row)
      const rowBusy = props.busyRowId === ids.candidateId
      const anyBusy = props.busyRowId !== null
      const canRetest = props.retestable ? props.retestable(row) : true
      return h(
        ResponsiveDropdown,
        {
          trigger: 'click',
          placement: 'bottom-end',
          triggerText: t('common.actions'),
          disabled: rowBusy,
          loading: rowBusy,
          height: 200,
          options: [
            { label: t('models.editCandidate'), key: 'edit' },
            ...(canRetest
              ? [
                  // Titled so the dropdown says what a retest actually does —
                  // it reruns all three probes and rewrites the stored
                  // verdicts. Optionally disabled while any retest is in
                  // flight: a single-flight runner would silently ignore a
                  // second click.
                  {
                    label: t('models.retest'),
                    key: 'retest',
                    disabled: props.disableRetestWhileBusy && anyBusy,
                    props: { title: t('models.retest_tip') },
                  },
                ]
              : []),
            { type: 'divider', key: 'd' },
            { label: t('models.deleteCandidate'), key: 'delete', props: { style: 'color: var(--color-danger)' } },
          ],
          onSelect: (key: string) => {
            if (key === 'edit') emit('edit', row, ids)
            else if (key === 'retest') emit('retest', row, ids)
            else if (key === 'delete') emit('delete', row, ids)
          },
        },
        {
          default: () =>
            h(
              NButton,
              { size: 'small', quaternary: true, circle: true, loading: rowBusy, disabled: rowBusy },
              { icon: () => h(MoreHorizontal, { size: 16 }) },
            ),
        },
      )
    },
  })

  return cols
})
</script>

<style scoped>
.candidate-filter {
  margin-bottom: var(--space-4);
}
</style>
