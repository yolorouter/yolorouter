// frontend/src/views/models/pricingBulkSelection.ts
//
// The bulk-selection pure layer, in the same spirit as the modelPricingGroups
// module: no Vue, no i18n — just the set algebra the checkboxes need. The
// selection is a Set of ids shared by every control; each function takes the
// current set and returns a NEW one, so the page can hold it in a ref and Vue
// sees a fresh value on every change. Consumer: the models list' bulk toolbar
// (model-level selection on a flat table — it uses the tri-state status, the
// row toggle, and the preview slot changes; its select-all is three lines of
// flat-Set algebra in the page).
//
// Scope rule: only models currently VISIBLE under the applied filter are ever
// touched. The ids themselves are what the bulk-price endpoint receives, so a
// selection made under one filter and confirmed under another stays exactly
// the rows the admin ticked.

import type { BillingMode, BulkPriceSlots } from '../../api/models'

/** A checkbox's three visual states (the models list' select-all reads it). */
export type GroupSelectionStatus = 'all' | 'partial' | 'none'

/** The tri-state over a flat id list — the shape a select-all sees (its
 *  scope is every visible row's id). */
export function selectionStatus(ids: number[], selected: ReadonlySet<number>): GroupSelectionStatus {
  let hits = 0
  for (const id of ids) {
    if (selected.has(id)) hits++
  }
  if (hits === 0) return 'none'
  if (hits === ids.length) return 'all'
  return 'partial'
}

/** (De)select one row: a fresh set, never mutating the input. */
export function toggleRow(rowId: number, selected: ReadonlySet<number>): Set<number> {
  const next = new Set(selected)
  if (!next.delete(rowId)) next.add(rowId)
  return next
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
 *  side reads as an em dash. The slot label keys live in the models domain:
 *  the flat models list is the page that renders them now. */
export function previewSlotChanges(billingMode: BillingMode | undefined, before: BulkPriceSlots, after: BulkPriceSlots): BulkSlotChange[] {
  const either = (b: number | null, a: number | null) => b !== null || a !== null
  const text = (v: number | null) => (v === null ? '—' : String(v))
  if (billingMode === 'audio') {
    if (!either(before.audio_unit_price, after.audio_unit_price)) return []
    return [{ labelKey: 'models.bulkSlotAudio', before: text(before.audio_unit_price), after: text(after.audio_unit_price) }]
  }
  if (billingMode !== 'token') return []
  const changes: BulkSlotChange[] = [
    { labelKey: 'models.bulkSlotInput', before: String(before.input_price), after: String(after.input_price) },
    { labelKey: 'models.bulkSlotOutput', before: String(before.output_price), after: String(after.output_price) },
  ]
  if (either(before.cache_write_price, after.cache_write_price)) {
    changes.push({
      labelKey: 'models.bulkSlotCacheWrite',
      before: text(before.cache_write_price),
      after: text(after.cache_write_price),
    })
  }
  if (either(before.cache_read_price, after.cache_read_price)) {
    changes.push({
      labelKey: 'models.bulkSlotCacheRead',
      before: text(before.cache_read_price),
      after: text(after.cache_read_price),
    })
  }
  return changes
}
