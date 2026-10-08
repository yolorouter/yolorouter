<!-- frontend/src/components/common/ResponsiveDataTable.vue
     A report table that adapts to viewport width, driven by ONE column
     definition:

       - Desktop: a naive NDataTable (horizontal scroll, tooltips, the usual).
       - Mobile: the same rows rendered as stacked cards — the first column is
         the card header (the dimension label), the remaining columns become
         label/value pairs. Reusing each column's `title` and `render` means a
         column change lands in both layouts at once; there is no second,
         card-specific definition to keep in sync.

     Controlled/pure: the parent owns columns + data. Breakpoint comes from the
     shared useIsMobile composable (MOBILE_BREAKPOINT). Empty and loading
     states on desktop are naive's; on mobile they're rendered here. Pass an
     #empty slot to control the empty message in both layouts. -->
<template>
  <!-- Desktop: the full data table. -->
  <NDataTable
    v-if="!isMobile"
    :columns="columns"
    :data="data"
    :loading="loading"
    :bordered="false"
    :single-line="false"
    :scroll-x="scrollX"
    :row-key="rowKey"
    :row-props="rowProps"
    :pagination="pagination"
    :remote="remote"
    :checked-row-keys="checkedRowKeys"
    size="small"
    @update:checked-row-keys="onCheckedKeys"
  >
    <template #empty>
      <slot name="empty" />
    </template>
  </NDataTable>

  <!-- Mobile: card list. -->
  <div v-else class="rdt-cards">
    <div v-if="loading" class="rdt-cards__status">
      <NSpin size="medium" />
    </div>

    <slot v-else-if="!data.length" name="empty" />

    <template v-else>
      <div
        v-for="row in data"
        :key="rowKey(row)"
        class="rdt-card"
        v-bind="rowProps ? rowProps(row) : {}"
      >
        <div v-if="headerColumn" class="rdt-card__header">
          <Cell :content="cellValue(headerColumn, row)" />
        </div>
        <div class="rdt-card__body">
          <div v-for="(col,index) of bodyColumns" :key="String(col.key)" class="rdt-card__field" :style="fieldSpanStyle(col, index)">
            <span class="rdt-card__label"><Cell :content="cellTitle(col)" /></span>
            <span class="rdt-card__value"><Cell :content="cellValue(col, row)" /></span>
          </div>
        </div>
      </div>

      <!-- Server-side paging still needs to be reachable on mobile — the
           desktop pager lives inside NDataTable, so render a standalone one
           here driven by the same pagination object. -->
      <div v-if="pagination && (pagination.itemCount ?? 0) > 10" class="rdt-cards__pager">
        <NPagination
          :page="pagination.page"
          :page-size="pagination.pageSize"
          :item-count="pagination.itemCount"
          :page-sizes="pagination.pageSizes"
          :show-size-picker="!isMobile"
          :page-slot="isMobile ? 7 : 9"
          @update:page="onMobilePage"
          @update:page-size="onMobilePageSize"
        />
      </div>
    </template>
  </div>
</template>

<script setup lang="ts" generic="Row extends Record<string, any>">
import { computed, type FunctionalComponent, type VNodeChild } from 'vue'
import { NDataTable, NPagination, NSpin, type DataTableColumns, type PaginationProps } from 'naive-ui'
import { useIsMobile } from '../../composables/useIsMobile'

// The subset of a naive column we read to build a card cell. Column groups
// (which have `children` instead of `key`/`render`) aren't used by the report
// tables, so this narrower shape is enough.
type Col = {
  key?: string | number
  title?: string | (() => VNodeChild)
  render?: (row: Row, index: number) => VNodeChild
}

const props = defineProps<{
  columns: DataTableColumns<Row>
  data: Row[]
  rowKey: (row: Row) => string | number
  loading?: boolean
  scrollX?: number
  // Optional NDataTable pass-throughs. When a list is server-side paged the
  // parent hands the same pagination object it would give NDataTable; on
  // mobile we drive a standalone NPagination from it. rowProps makes rows
  // clickable in both layouts (the whole card becomes the tap target).
  pagination?: PaginationProps | false
  remote?: boolean
  rowProps?: (row: Row) => Record<string, unknown>
  // Keys of columns whose card field must span the full card width — for
  // panel-like content (an expand summary) that cannot share a half-width
  // cell with a sibling field.
  fullSpanKeys?: (string | number)[]
  // Selection pass-through for tables whose columns carry a `type:
  // 'selection'` entry: the parent stays the owner of the checked keys
  // (v-model:checked-row-keys), exactly as it would with a bare NDataTable.
  // Desktop only — the mobile card layout has no checkboxes, so parents that
  // need selection gate the column themselves by isMobile.
  checkedRowKeys?: Array<string | number>
}>()

const emit = defineEmits<{
  'update:checkedRowKeys': [keys: Array<string | number>]
}>()

const isMobile = useIsMobile()

// NDataTable hands (keys, rows, meta); the parent already owns the rows, so
// only the keys travel on.
function onCheckedKeys(keys: Array<string | number>) {
  emit('update:checkedRowKeys', keys)
}

// Renders an already-built VNodeChild (a column's title or cell output) inline.
// A tiny functional component so the template can drop naive/HelpLabel VNodes
// straight into the card without re-implementing them.
const Cell: FunctionalComponent<{ content: VNodeChild }> = (slotProps) => slotProps.content

const cols = computed<Col[]>(() => props.columns as unknown as Col[])

// First column is the dimension label (model / provider / caller / bucket) —
// promoted to the card header. The rest become label/value rows.
const headerColumn = computed<Col | undefined>(() => cols.value[0])
const bodyColumns = computed<Col[]>(() => {
  const arr = cols.value.slice(1)
  return arr
})

function cellTitle(col: Col): VNodeChild {
  return typeof col.title === 'function' ? col.title() : col.title
}

// A field spans the full card width when the parent asked for it by key, or
// when it is an unpaired last field (spanning it avoids a lone half-width
// cell at the bottom of the card).
function fieldSpanStyle(col: Col, index: number): string {
  const full =
    (props.fullSpanKeys ?? []).includes(col.key as string | number) ||
    (bodyColumns.value.length === index + 1 && bodyColumns.value.length % 2 === 1)
  return full ? 'grid-column: 1 / -1;' : ''
}

function cellValue(col: Col, row: Row): VNodeChild {
  if (col.render) return col.render(row, 0)
  return col.key != null ? (row[col.key as keyof Row] as VNodeChild) : null
}

// Mobile pager → forward to whichever callbacks the pagination object carries.
// NDataTable pagination objects use onChange for page and onUpdatePageSize for
// size; support onUpdatePage too so either convention works. naive types each
// handler as MaybeArray<fn>, so normalize to an array before invoking.
function callHandlers<A>(handler: ((arg: A) => void) | ((arg: A) => void)[] | undefined, arg: A) {
  if (!handler) return
  const fns = Array.isArray(handler) ? handler : [handler]
  for (const fn of fns) fn(arg)
}

function onMobilePage(p: number) {
  const pg = props.pagination
  if (!pg) return
  callHandlers(pg.onChange, p)
  callHandlers(pg.onUpdatePage, p)
}

function onMobilePageSize(ps: number) {
  const pg = props.pagination
  if (!pg) return
  callHandlers(pg.onUpdatePageSize, ps)
}
</script>

<style scoped lang="less">
.rdt-cards {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.rdt-cards__status {
  display: flex;
  justify-content: center;
  padding: var(--space-6) 0;
}

.rdt-cards__pager {
  display: flex;
  justify-content: center;
  padding: var(--space-3) 0 var(--space-1);
}

.rdt-card {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg, 12px);
  overflow: hidden;
  padding-bottom: var(--space-2);
}

.rdt-card__header {
  font-size: var(--text-base);
  font-weight: 700;
  color: #6467f2;
  overflow-wrap: anywhere;
  padding: var(--space-3) var(--space-4) 0;
}

.rdt-card__body {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
}

.rdt-card__field {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.rdt-card__label {
  font-size: var(--text-xs);
  color: var(--color-text-muted);
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
  background-color: #f8fafd !important;
  padding: var(--space-3);
  height: 44px;
}

.rdt-card__value {
  font-size: var(--text-sm);
  font-weight: 600;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
  overflow-wrap: anywhere;
  padding: var(--space-3)
}

:deep(.mono-cell) {
  font-family: var(--font-mono, monospace);
  font-weight: 600;
  color: var(--color-text);
}

:deep(.rdt-card__header span) {
  color: #6467f2 !important
}

@media (max-width: @mobile-breakpoint) {
  .rdt-cards__pager {
    display: flex;
    justify-content: center;
    padding: var(--space-3) 0 ;
  }
}
</style>
