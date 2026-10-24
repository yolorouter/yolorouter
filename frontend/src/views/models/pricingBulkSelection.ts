// frontend/src/views/models/pricingBulkSelection.ts
//
// The model pricing page's bulk-selection pure layer, in the same spirit as
// the modelPricingGroups module: no Vue, no i18n — just the set algebra the
// page's checkboxes need. The selection is a Set of candidate ids shared by
// every control (row, group header, select-all); each function takes the
// current set and returns a NEW one, so the page can hold it in a ref and
// Vue sees a fresh value on every change.
//
// Scope rule: only rows currently VISIBLE under the applied filter are ever
// touched. A select-all completes the visible set (keeping ids selected
// before a filter narrowed the view — they stay selected, they just are not
// re-toggled), a group toggle adds or removes exactly that group's visible
// rows. The ids themselves are what the bulk-price endpoint receives, so a
// selection made under one filter and confirmed under another stays exactly
// the rows the admin ticked.

import type { BillingMode, BulkPriceSlots } from '../../api/models'
import type { PricingGroup, PricingRow } from './modelPricingGroups'

/** The group header checkbox's three visual states. */
export type GroupSelectionStatus = 'all' | 'partial' | 'none'

/** Flat candidate ids of every currently visible row, groups in render
 *  order — the select-all's scope and the bulk call's id source. */
export function visibleRowIds(groups: PricingGroup[]): number[] {
  return groups.flatMap((group) => group.rows.map((row) => row.candidate.id))
}

/** The tri-state over a flat id list — the shape a select-all sees (its
 *  scope is every visible row's id, not any one group's rows). */
export function selectionStatus(ids: number[], selected: ReadonlySet<number>): GroupSelectionStatus {
  let hits = 0
  for (const id of ids) {
    if (selected.has(id)) hits++
  }
  if (hits === 0) return 'none'
  if (hits === ids.length) return 'all'
  return 'partial'
}

/** One group's tri-state over the shared selection, computed against the
 *  group's currently visible rows only. */
export function groupSelectionStatus(rows: PricingRow[], selected: ReadonlySet<number>): GroupSelectionStatus {
  return selectionStatus(rows.map((row) => row.candidate.id), selected)
}

/** (De)select one row: a fresh set, never mutating the input. */
export function toggleRow(rowId: number, selected: ReadonlySet<number>): Set<number> {
  const next = new Set(selected)
  if (!next.delete(rowId)) next.add(rowId)
  return next
}

/** One group header click. checked=true (the header showing none or partial)
 *  adds the group's visible rows; checked=false (the header showing all)
 *  removes them. Ids outside the group are left alone. */
export function toggleGroup(rows: PricingRow[], checked: boolean, selected: ReadonlySet<number>): Set<number> {
  const next = new Set(selected)
  for (const row of rows) {
    if (checked) next.add(row.candidate.id)
    else next.delete(row.candidate.id)
  }
  return next
}

/** The select-all click over the current visible scope. checked=true adds
 *  every visible row id (ids selected before the filter narrowed the view
 *  stay selected); checked=false removes exactly the visible ids — a
 *  selection made under a narrower filter is untouched by the widening
 *  sweep's clear. */
export function toggleSelectAll(groups: PricingGroup[], checked: boolean, selected: ReadonlySet<number>): Set<number> {
  return toggleGroup(groups.flatMap((group) => group.rows), checked, selected)
}

/** One preview row's before→after entry as the drawer renders it. */
export interface BulkSlotChange {
  labelKey: string
  before: string
  after: string
}

/** The slot entries one CHANGED preview row shows, family per the row's
 *  billing mode (the endpoint reports raw slot snapshots without naming the
 *  mode; the page resolves it from its own data):
 *
 *    - token rows report the four token slots — the cache pair only when
 *      either side carries a value (a null→value or value→null move is a
 *      real change the admin must see);
 *    - audio rows report the character price alone;
 *    - anything else (tier rows, a row deleted mid-flow and therefore
 *      unknown to the page) reports nothing — for those the skip reason is
 *      the whole story.
 *
 *  Values render as the bare numbers the backend sent, exactly like the
 *  mapping tables' price columns — no rounding invented for display. A null
 *  side reads as an em dash. */
export function previewSlotChanges(billingMode: BillingMode | undefined, before: BulkPriceSlots, after: BulkPriceSlots): BulkSlotChange[] {
  const either = (b: number | null, a: number | null) => b !== null || a !== null
  const text = (v: number | null) => (v === null ? '—' : String(v))
  if (billingMode === 'audio') {
    if (!either(before.audio_unit_price, after.audio_unit_price)) return []
    return [{ labelKey: 'modelPricing.bulkSlotAudio', before: text(before.audio_unit_price), after: text(after.audio_unit_price) }]
  }
  if (billingMode !== 'token') return []
  const changes: BulkSlotChange[] = [
    { labelKey: 'modelPricing.bulkSlotInput', before: String(before.input_price), after: String(after.input_price) },
    { labelKey: 'modelPricing.bulkSlotOutput', before: String(before.output_price), after: String(after.output_price) },
  ]
  if (either(before.cache_write_price, after.cache_write_price)) {
    changes.push({
      labelKey: 'modelPricing.bulkSlotCacheWrite',
      before: text(before.cache_write_price),
      after: text(after.cache_write_price),
    })
  }
  if (either(before.cache_read_price, after.cache_read_price)) {
    changes.push({
      labelKey: 'modelPricing.bulkSlotCacheRead',
      before: text(before.cache_read_price),
      after: text(after.cache_read_price),
    })
  }
  return changes
}
