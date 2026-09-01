import { VERIFICATION_UNTESTED } from '../api/candidateStatus'
import type { ImportModelItemInput, SuggestedPrice } from '../api/models'
import { suggestsImageOutput } from './imageModelNames'

// One row of the bulk-import dialog: an upstream model id plus its selection,
// editable prices, and the modality the row will import under. Rows for models
// this provider already maps are "added"; among those, a mapping still waiting
// on a probe verdict is additionally "unfinished" and stays selectable —
// re-submitting it is how probes lost to a restart get requeued (the probe
// queue is not durable). A settled added row can never be selected again.
export interface ImportRow {
  name: string
  added: boolean
  unfinished: boolean
  checked: boolean
  // The single-select form of the output-modalities declaration the row
  // submits: 'both' is the ['text','image'] pair. Default 'text' — the same
  // default every undeclared import gets server-side.
  modality: ImportRowModality
  priceSource: SuggestedPrice['source']
  inputPrice: number | null
  outputPrice: number | null
  cacheWritePrice: number | null
  cacheReadPrice: number | null
}

export type ImportRowModality = 'text' | 'image' | 'both'

// modalitiesFor maps the select's single value onto the declaration list the
// request sends.
export function modalitiesFor(modality: ImportRowModality): string[] {
  if (modality === 'image') return ['image']
  if (modality === 'both') return ['text', 'image']
  return ['text']
}

// The dialog's hard length cap for catalog ids. Bulk import publishes the
// upstream id verbatim as the PUBLIC model name, whose server-side limit is
// 100 characters — a longer id can never import (the server skips it per
// item), so offering it as a selectable row would only ever produce a
// confusing skip. This is tighter than the 200-char transport cap on the
// batch endpoints, so it also keeps every request well-formed. Names that are
// merely invalid by the server's naming RULES (characters, slash placement)
// still go through: the import skips those per item, and mirroring that rule
// here would just drift.
const MAX_IMPORTABLE_NAME_LENGTH = 100

export function isTransportableModelName(name: string): boolean {
  const trimmed = name.trim()
  return trimmed !== '' && trimmed.length <= MAX_IMPORTABLE_NAME_LENGTH
}

// normalizeCatalogNames maps a raw upstream catalog to the exact strings the
// dialog may put in ANY request: trimmed, non-blank, within the transport cap.
// Trimming must happen BEFORE the length check and the value sent must be the
// trimmed one — a padded id whose raw length exceeds the cap but whose trimmed
// length fits would otherwise pass the filter and still 400 the whole call.
export function normalizeCatalogNames(names: string[]): string[] {
  return names.map((n) => n.trim()).filter(isTransportableModelName)
}

// The slice of a provider-candidate row this dialog needs to classify an
// existing mapping: its model name, and whether its verification has settled.
export interface ExistingMapping {
  model_name: string
  verification_status: number
}

// buildImportRows turns the fetched upstream catalogue into dialog rows:
// names are trimmed and deduplicated, already-mapped models become "added"
// rows, and rows with a price suggestion are pre-checked and prefilled
// — the smart default that selects mainstream models while leaving embedding /
// deprecated entries unchecked. Unfinished added rows (mapping stored, verdict
// still Untested) stay selectable for recovery but are NOT pre-checked: see
// the note at their construction. A name the image-family heuristic recognizes
// preselects the image modality — an override, not a verdict, and only a
// starting point the admin can flip per row.
export function buildImportRows(
  upstreamNames: string[],
  existing: ExistingMapping[],
  prices: Record<string, SuggestedPrice>,
): ImportRow[] {
  const existingByName = new Map(existing.map((c) => [c.model_name, c]))
  const seen = new Set<string>()
  const rows: ImportRow[] = []
  for (const raw of upstreamNames) {
    const name = raw.trim()
    if (!isTransportableModelName(name) || seen.has(name)) continue
    seen.add(name)
    const mapped = existingByName.get(name)
    if (mapped !== undefined) {
      const unfinished = mapped.verification_status === VERIFICATION_UNTESTED
      // Unfinished rows are selectable but deliberately NOT pre-checked: an
      // untested mapping is not necessarily lost queue work — an admin may
      // have saved it disabled on purpose, and re-probing it auto-enables on
      // a pass. Checking one is the admin's explicit request to (re)probe.
      // Their modality stays 'text' because the declaration column is inert
      // here — the server derives an existing model's billing from the model
      // row it already has, never from the re-queued submission.
      rows.push({ name, added: true, unfinished, checked: false, modality: 'text', priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null })
      continue
    }
    const suggestion = prices[name]
    const hasPrice = suggestion !== undefined && suggestion.source !== ''
    rows.push({
      name,
      added: false,
      unfinished: false,
      checked: hasPrice,
      modality: suggestsImageOutput(name) ? 'image' : 'text',
      priceSource: hasPrice ? suggestion.source : '',
      inputPrice: hasPrice ? suggestion.input_price : null,
      outputPrice: hasPrice ? suggestion.output_price : null,
      cacheWritePrice: hasPrice ? suggestion.cache_write_price : null,
      cacheReadPrice: hasPrice ? suggestion.cache_read_price : null,
    })
  }
  return rows
}

// One import request carries at most this many items (the server rejects
// larger payloads outright), so a bigger selection goes up as several
// sequential requests — see chunkByCap.
export const IMPORT_BATCH_CAP = 2000

// chunkByCap splits a payload into transport-sized batches, preserving
// order. Each batch commits atomically server-side; splitting trades the
// whole-selection atomicity (which the transport cap makes impossible anyway)
// for a select-all that actually works on catalogs larger than one request.
// Generic because two transports share the same cap discipline: the import
// payload and the suggest-prices name list.
export function chunkByCap<T>(items: T[], cap: number): T[][] {
  const chunks: T[][] = []
  for (let i = 0; i < items.length; i += cap) {
    chunks.push(items.slice(i, i + cap))
  }
  return chunks
}

// toImportItems builds the request payload from the dialog state: rows the
// admin left checked, where "checked" covers new rows and unfinished added
// rows alike — the server skips an existing mapping but returns its candidate
// id so its lost probe is requeued. Prices left blank go as zero — the
// "unpriced but importable" contract (an existing mapping's stored prices are
// untouched by the skip). A new row carries its modality declaration; an
// added row OMITS the field entirely — the server treats a present
// declaration on an existing model as a claim that must match the stored
// one, and the requeue path must not submit a claim it never made (the
// modality select is inert on added rows, so whatever it shows is not a
// declaration).
export function toImportItems(rows: ImportRow[]): ImportModelItemInput[] {
  return rows
    .filter((r) => r.checked && (!r.added || r.unfinished))
    .map((r) => ({
      provider_model_name: r.name,
      ...(r.added ? {} : { output_modalities: modalitiesFor(r.modality) }),
      input_price: r.inputPrice ?? 0,
      output_price: r.outputPrice ?? 0,
      cache_write_price: r.cacheWritePrice,
      cache_read_price: r.cacheReadPrice,
    }))
}
