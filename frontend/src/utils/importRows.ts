import { VERIFICATION_UNTESTED } from '../api/candidateStatus'
import type { ImportModelItemInput, SuggestedPrice } from '../api/models'
import { suggestsImageOutput } from './imageModelNames'

// One row of the bulk-import dialog: an upstream model id plus its selection,
// editable prices, and the capability declarations the row will submit. Rows
// for models this provider already maps are "added"; among those, a mapping
// still waiting on a probe verdict is additionally "unfinished" and stays
// selectable — re-submitting it is how probes lost to a restart get requeued
// (the probe queue is not durable). A settled added row can never be selected
// again.
//
// Every capability slot is tri-state by absence: null / 0 / '' means
// UNDECLARED, and an undeclared slot stays off the wire (see toImportItems).
// Only the six capability fields below flip a row's status — the public name
// and the output axes do not (see statusOf).
export interface ImportRow {
  name: string
  added: boolean
  unfinished: boolean
  checked: boolean
  // The OUTPUT axes the row declares, as the vocabulary the server stores
  // ("text", "image", "audio"; video is not importable here). Default
  // ['text'] — the same default every undeclared import gets server-side.
  // The single-select column shows the ImportRowModality shorthand of this
  // set ('both' is the ['text','image'] pair) until the column is redesigned.
  out: string[]
  // The INPUT axes declaration; null = undeclared — nothing preselects it
  // (the image/audio heuristics speak OUTPUT only), and an absent list must
  // never be read as "text only" server-side either.
  inMods: string[] | null
  // The public model name the row creates under; '' = publish the upstream
  // id verbatim. An added row carries the stored mapping's public name here,
  // which is what its requeue submission must address the mapping by.
  pubName: string
  // Model-level capability declarations; null = undeclared.
  ctxLength: number | null
  maxOut: number | null
  temp: number | null
  // The tri-state selects: 0 = undeclared, 1 = supported, 2 = unsupported.
  tools: 0 | 1 | 2
  reasoning: 0 | 1 | 2
  priceSource: SuggestedPrice['source']
  inputPrice: number | null
  outputPrice: number | null
  cacheWritePrice: number | null
  cacheReadPrice: number | null
  // The single price an audio row carries in place of the token slots.
  audioUnitPrice: number | null
}

// The single-select shorthand of an output-axes set, kept only as the import
// dialog's transitional display vocabulary (the column is redesigned later;
// the row's declaration itself is the `out` array).
export type ImportRowModality = 'text' | 'image' | 'both' | 'audio'

// modalitiesFor maps the select's single value onto the declaration list the
// request sends.
export function modalitiesFor(modality: ImportRowModality): string[] {
  if (modality === 'image') return ['image']
  if (modality === 'both') return ['text', 'image']
  if (modality === 'audio') return ['audio']
  return ['text']
}

// modalityOf derives the display shorthand back from a row's axes. Anything
// that is not exactly one of the four shorthand shapes reads as 'text' — the
// same default the server resolves an absent declaration to.
export function modalityOf(out: string[]): ImportRowModality {
  if (out.length === 2 && out.includes('text') && out.includes('image')) return 'both'
  if (out.length === 1 && out[0] === 'image') return 'image'
  if (out.length === 1 && out[0] === 'audio') return 'audio'
  return 'text'
}

// How a row reads on the two-state configured/unconfigured axis. A row is
// configured when it IS an existing mapping (its capabilities live on the
// stored model, not on the row) or when ANY of the six capability fields
// carries a declaration. The public name and the output axes never count:
// both have fully default-shaped answers (the upstream id, text-only) that a
// plain re-import produces without the admin knowing anything about the
// model — calling that "configured" would light the marker on untouched
// catalogs.
export type ImportRowStatus = 'unconfigured' | 'configured'

export function statusOf(row: ImportRow): ImportRowStatus {
  if (row.added) return 'configured'
  if (row.inMods !== null) return 'configured'
  if (row.ctxLength !== null || row.maxOut !== null || row.temp !== null) return 'configured'
  if (row.tools !== 0 || row.reasoning !== 0) return 'configured'
  return 'unconfigured'
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
// existing mapping: which upstream id the mapping was stored under (the
// match key — a mapping created under a custom public name must still be
// recognized when its upstream id reappears in the catalog), the public name
// the mapping lives under, and whether its verification has settled.
export interface ExistingMapping {
  model_name: string
  provider_model_name: string
  verification_status: number
}

// buildImportRows turns the fetched upstream catalogue into dialog rows:
// names are trimmed and deduplicated, already-mapped models become "added"
// rows, and rows with a price suggestion are pre-checked and prefilled
// — the smart default that selects mainstream models while leaving embedding /
// deprecated entries unchecked. Unfinished added rows (mapping stored, verdict
// still Untested) stay selectable for recovery but are NOT pre-checked: see
// the note at their construction. A name the image-family heuristic recognizes
// preselects the image OUTPUT axis — an override, not a verdict, and only a
// starting point the admin can flip per row. The input axes are never
// prefilled: the heuristics are output heuristics, and an input declaration
// is the admin's own knowledge, not a guess.
export function buildImportRows(
  upstreamNames: string[],
  existing: ExistingMapping[],
  prices: Record<string, SuggestedPrice>,
): ImportRow[] {
  // Degenerate-shape defense: a custom-named re-import can map several public
  // models onto the SAME provider_model_name. A plain `new Map(pairs)` keeps
  // the LAST duplicate, so the added row's requeue target (pubName) would
  // depend on whatever order the candidates endpoint happened to return.
  // First-seen wins instead: deterministic, order-stable, and no mapping is
  // silently picked at random.
  const existingByName = new Map<string, ExistingMapping>()
  for (const c of existing) {
    if (!existingByName.has(c.provider_model_name)) existingByName.set(c.provider_model_name, c)
  }
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
      // Every declaration slot stays unset — an added row's capabilities are
      // server-owned, and its submission carries none of them (see
      // toImportItems). pubName holds the mapping's public model name so the
      // requeue submission can address the mapping when the upstream id is
      // not itself the public name.
      rows.push({
        name, added: true, unfinished, checked: false,
        out: ['text'], inMods: null, pubName: mapped.model_name,
        ctxLength: null, maxOut: null, temp: null, tools: 0, reasoning: 0,
        priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null, audioUnitPrice: null,
      })
      continue
    }
    const suggestion = prices[name]
    const hasPrice = suggestion !== undefined && suggestion.source !== ''
    // An audio suggestion is the audio signal: the seed catalog's audio
    // section covers exactly the speech models, so a hit preselects the
    // audio output axis and prefills the one price the row carries — the
    // name-heuristic image preselect works the same way, an override the
    // admin can flip per row.
    const isAudio = hasPrice && suggestion.audio_unit_price != null
    rows.push({
      name,
      added: false,
      unfinished: false,
      checked: hasPrice,
      out: isAudio ? ['audio'] : suggestsImageOutput(name) ? ['image'] : ['text'],
      inMods: null,
      pubName: '',
      ctxLength: null,
      maxOut: null,
      temp: null,
      tools: 0,
      reasoning: 0,
      priceSource: hasPrice ? suggestion.source : '',
      inputPrice: hasPrice && !isAudio ? suggestion.input_price : null,
      outputPrice: hasPrice && !isAudio ? suggestion.output_price : null,
      cacheWritePrice: hasPrice && !isAudio ? suggestion.cache_write_price : null,
      cacheReadPrice: hasPrice && !isAudio ? suggestion.cache_read_price : null,
      audioUnitPrice: isAudio ? suggestion.audio_unit_price : null,
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
// id so its lost probe is requeued. The wire is omit-shaped end to end: an
// undeclared capability field stays off the item entirely (the seven default
// fields — model_name, input_modalities, context_length, supports_tools,
// supports_reasoning, temperature, max_output — go out only when the row
// declares them), and a BLANK price stays off too — "empty = unpriced", the
// untouched-marker the server's PATCH-shaped price fields read. Zeros are
// never invented for blanks.
//
// An added row (the unfinished requeue included) submits ONLY names: its
// stored public model name as model_name, and no declarations of any kind —
// the server reads a present declaration on an existing model as a claim that
// must match the stored one, and the requeue path must not submit claims it
// never made. pubName is always stamped by buildImportRows; a hand-built row
// that leaves it '' sends the empty string, which the server rejects per item
// — loud, rather than silently addressing a differently-named model.
export function toImportItems(rows: ImportRow[]): ImportModelItemInput[] {
  return rows
    .filter((r) => r.checked && (!r.added || r.unfinished))
    .map((r): ImportModelItemInput => {
      if (r.added) {
        return {
          provider_model_name: r.name,
          model_name: r.pubName,
        }
      }
      // An audio-declared row bills per character: its single price replaces
      // the token slots, which stay off the wire entirely — under audio
      // billing the server keeps them inert, and the dialog never collects
      // them for such a row.
      const audio = modalityOf(r.out) === 'audio'
      const item: ImportModelItemInput = {
        provider_model_name: r.name,
        output_modalities: [...r.out],
      }
      // put adds a field only when the row actually declares it — an omitted
      // field must be ABSENT on the item, not present-with-undefined, so the
      // payload's shape is inspectable and "empty = unpriced" is literal.
      const put = <K extends keyof ImportModelItemInput>(key: K, value: ImportModelItemInput[K] | undefined) => {
        if (value !== undefined) item[key] = value
      }
      put('model_name', r.pubName === '' ? undefined : r.pubName)
      put('input_modalities', r.inMods === null ? undefined : [...r.inMods])
      if (audio) {
        put('audio_unit_price', r.audioUnitPrice ?? undefined)
      } else {
        put('input_price', r.inputPrice ?? undefined)
        put('output_price', r.outputPrice ?? undefined)
        put('cache_write_price', r.cacheWritePrice ?? undefined)
        put('cache_read_price', r.cacheReadPrice ?? undefined)
      }
      put('context_length', r.ctxLength ?? undefined)
      put('max_output', r.maxOut ?? undefined)
      put('supports_tools', r.tools === 0 ? undefined : r.tools === 1 ? 'yes' : 'no')
      put('supports_reasoning', r.reasoning === 0 ? undefined : r.reasoning === 1 ? 'yes' : 'no')
      put('temperature', r.temp ?? undefined)
      return item
    })
}
