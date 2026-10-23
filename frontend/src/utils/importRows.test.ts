import { describe, expect, it } from 'vitest'
import { buildImportRows, chunkByCap, isTransportableModelName, normalizeCatalogNames, statusOf, toImportItems, type ImportRow } from './importRows'

const price = (input: number, output: number, source: 'history' | 'seed', audio: number | null = null) => ({
  input_price: input,
  output_price: output,
  cache_write_price: null,
  cache_read_price: null,
  audio_unit_price: audio,
  source,
  catalog_updated_at: '',
})

// An existing mapping as the candidates endpoint reports it: settled rows hold
// a verdict (verification_status 1/2), unfinished ones are still Untested (0).
// provider defaults to the public name — the pre-custom-naming shape, where
// the upstream id and the model name coincide.
const mapped = (name: string, verification: number, provider = name) => ({
  model_name: name,
  provider_model_name: provider,
  verification_status: verification,
})

// A fresh dialog row with every declaration unset — the baseline the
// toImportItems and statusOf fixtures mutate from.
const row = (overrides: Partial<ImportRow> = {}): ImportRow => ({
  name: 'm',
  added: false,
  unfinished: false,
  checked: true,
  out: ['text'],
  inMods: null,
  pubName: '',
  ctxLength: null,
  maxOut: null,
  tools: 0,
  reasoning: 0,
  temp: null,
  priceSource: '',
  inputPrice: null,
  outputPrice: null,
  cacheWritePrice: null,
  cacheReadPrice: null,
  audioUnitPrice: null,
  ...overrides,
})

describe('buildImportRows', () => {
  it('checks price-suggested rows by default and prefills their prices', () => {
    const rows = buildImportRows(['deepseek-v4', 'obscure-embedding'], [], {
      'deepseek-v4': price(2, 8, 'seed'),
    })
    expect(rows).toHaveLength(2)
    const [priced, unpriced] = rows
    expect(priced).toMatchObject({ name: 'deepseek-v4', added: false, checked: true, priceSource: 'seed', inputPrice: 2, outputPrice: 8 })
    // No suggestion: every price stays BLANK (null = unpriced), never a
    // padded zero — the wire omits blank prices instead of claiming free.
    expect(unpriced).toMatchObject({ name: 'obscure-embedding', checked: false, priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null, audioUnitPrice: null })
  })

  it('preselects the audio axis and prefills its single price on an audio suggestion', () => {
    const rows = buildImportRows(['speech-2.8-hd'], [], {
      'speech-2.8-hd': price(0, 0, 'seed', 350),
    })
    expect(rows[0]).toMatchObject({
      out: ['audio'],
      checked: true,
      inputPrice: null,
      outputPrice: null,
      audioUnitPrice: 350,
    })
  })

  it('leaves the input axes undeclared on heuristic rows', () => {
    // The image/audio preselections speak the OUTPUT axis only; an input
    // declaration is the admin's knowledge, never a guess — absent stays
    // absent on the wire too.
    const rows = buildImportRows(['wan2.7-image-pro', 'speech-2.8-hd', 'deepseek-v4'], [], {
      'speech-2.8-hd': price(0, 0, 'seed', 350),
    })
    expect(rows.map((r) => r.inMods)).toEqual([null, null, null])
  })

  it('marks settled mappings as added and never checks them', () => {
    const rows = buildImportRows(['deepseek-v4'], [mapped('deepseek-v4', 1)], {
      'deepseek-v4': price(2, 8, 'history'),
    })
    expect(rows[0]).toMatchObject({ name: 'deepseek-v4', added: true, unfinished: false, checked: false })
  })

  it('keeps unfinished mappings selectable but not pre-checked', () => {
    // Untested rows may be lost queue work (restart) — but they may equally be
    // mappings an admin saved disabled on purpose, and re-probing auto-enables
    // on a pass. Recovery therefore requires an explicit check, never a
    // default one.
    const rows = buildImportRows(['deepseek-v4'], [mapped('deepseek-v4', 0)], {})
    expect(rows[0]).toMatchObject({ name: 'deepseek-v4', added: true, unfinished: true, checked: false })
  })

  it('matches existing mappings by provider_model_name, so a custom-named mapping reads as added', () => {
    // The upstream id is the match key, not the public model name: a mapping
    // stored under a custom name must still be recognized (added, not a fresh
    // row) when its upstream id reappears in the catalog. The row carries the
    // stored public name — what its requeue submission must address.
    const rows = buildImportRows(['claude-sonnet-x'], [mapped('sonnet', 0, 'claude-sonnet-x')], {})
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({ name: 'claude-sonnet-x', added: true, unfinished: true, checked: false, pubName: 'sonnet' })
    // The pre-custom-naming shape (upstream id = public name) is unchanged.
    const sameName = buildImportRows(['deepseek-v4'], [mapped('deepseek-v4', 1)], {})
    expect(sameName[0]).toMatchObject({ name: 'deepseek-v4', added: true, pubName: 'deepseek-v4' })
  })

  it('keeps the first mapping when several share one provider_model_name', () => {
    // Degenerate shape: a custom-named re-import can map two public models
    // onto the SAME upstream id. The row must still read as added, addressing
    // the FIRST mapping seen — never crash, and never let the requeue target
    // depend on the order the candidates endpoint returned.
    const rows = buildImportRows(
      ['claude-sonnet-x'],
      [mapped('sonnet', 1, 'claude-sonnet-x'), mapped('sonnet-alias', 0, 'claude-sonnet-x')],
      {},
    )
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({ name: 'claude-sonnet-x', added: true, unfinished: false, pubName: 'sonnet' })
  })

  it('trims names and drops duplicates and blanks from the upstream list', () => {
    const rows = buildImportRows([' deepseek-v4 ', 'deepseek-v4', '', '  '], [], {})
    expect(rows.map((r) => r.name)).toEqual(['deepseek-v4'])
  })

  it('normalizes catalog ids to their transportable form', () => {
    // What gets SENT must be the same trimmed value the filter judged: a
    // padded id whose raw length exceeds the cap but whose trimmed length
    // fits would otherwise pass the filter and still 400 the request.
    const paddedOverlong = ' ' + 'x'.repeat(99) + ' '.repeat(10)
    expect(normalizeCatalogNames([' deepseek-v4 ', paddedOverlong, 'x'.repeat(101), '  '])).toEqual([
      'deepseek-v4',
      'x'.repeat(99),
    ])
  })

  it('drops catalog ids the API cannot accept in any request', () => {
    // A single over-long garbage id would 400 the WHOLE suggest-prices or
    // import call (the transport layer caps names at 200 chars), turning one
    // bad catalog entry into a dead dialog. Such ids can never be imported, so
    // they are dropped up front rather than offered as selectable rows.
    // The cap is the PUBLIC model name limit (100): bulk import publishes the
    // upstream id verbatim as the model name, so a longer id can never import
    // and would only ever produce a confusing per-item skip.
    const overlong = 'x'.repeat(101)
    const rows = buildImportRows([overlong, 'deepseek-v4'], [], {})
    expect(rows.map((r) => r.name)).toEqual(['deepseek-v4'])
    expect(isTransportableModelName(overlong)).toBe(false)
    expect(isTransportableModelName('x'.repeat(100))).toBe(true)
    expect(isTransportableModelName('  ')).toBe(false)
  })
})

describe('chunkByCap', () => {
  it('splits a payload into transport-sized batches, keeping order', () => {
    // The import endpoint caps one request at 2000 items; a select-all on a
    // larger catalog must go up as several compliant requests, not one 400.
    const items = Array.from({ length: 5 }, (_, i) => ({ provider_model_name: `m-${i}` }))
    expect(chunkByCap(items, 2).map((c) => c.map((x) => x.provider_model_name))).toEqual([
      ['m-0', 'm-1'],
      ['m-2', 'm-3'],
      ['m-4'],
    ])
    expect(chunkByCap([], 2)).toEqual([])
    expect(chunkByCap(items, 10)).toEqual([items])
  })

  it('splits plain name lists the same way, so price suggestions cover every chunk', () => {
    // The suggest-prices endpoint shares the 2000-per-request cap; a catalog
    // larger than the cap must have its names batched too, or every row
    // past the cap silently loses its suggestion.
    expect(chunkByCap(['a', 'b', 'c'], 2)).toEqual([['a', 'b'], ['c']])
  })
})

describe('toImportItems', () => {
  it('submits checked new rows and checked unfinished rows, omitting blank prices', () => {
    const rows: ImportRow[] = [
      row({ name: 'a', priceSource: 'seed', inputPrice: 2, outputPrice: 8, cacheWritePrice: 0.5 }),
      // Blank prices stay off the wire — empty = unpriced, never a zero.
      row({ name: 'b' }),
      row({ name: 'c', checked: false }),
      row({ name: 'd', added: true, unfinished: false, checked: false, pubName: 'd' }),
      // A checked unfinished mapping goes in the payload: the server skips it
      // as existing but hands back its candidate id for requeueing. Names
      // only — no declarations of any kind.
      row({ name: 'e', added: true, unfinished: true, checked: true, pubName: 'e' }),
    ]
    expect(toImportItems(rows)).toStrictEqual([
      { provider_model_name: 'a', output_modalities: ['text'], input_price: 2, output_price: 8, cache_write_price: 0.5 },
      { provider_model_name: 'b', output_modalities: ['text'] },
      // An added row carries NO declaration: the server reads a present
      // declaration on an existing model as a claim that must match the
      // stored one, and the requeue path must not submit claims (the whole
      // declaration column is inert on added rows).
      { provider_model_name: 'e', model_name: 'e' },
    ])
  })

  it('sends filled prices as numbers and omits blank ones entirely (TC-01)', () => {
    // toStrictEqual: an undeclared field must be ABSENT, not present as null,
    // zero, or undefined.
    expect(toImportItems([
      row({ name: 'prefilled', priceSource: 'seed', inputPrice: 2, outputPrice: 8, cacheWritePrice: 0.5, cacheReadPrice: null }),
    ])).toStrictEqual([
      { provider_model_name: 'prefilled', output_modalities: ['text'], input_price: 2, output_price: 8, cache_write_price: 0.5 },
    ])
  })

  it('sends only the names on an unpriced default row (TC-01)', () => {
    expect(toImportItems([row({ name: 'unpriced' })])).toStrictEqual([
      { provider_model_name: 'unpriced', output_modalities: ['text'] },
    ])
  })

  it('sends the character price and omits the token prices on an audio row (TC-01)', () => {
    expect(toImportItems([row({ name: 'speech-2.8-hd', out: ['audio'], priceSource: 'seed', audioUnitPrice: 350 })])).toStrictEqual([
      { provider_model_name: 'speech-2.8-hd', output_modalities: ['audio'], audio_unit_price: 350 },
    ])
  })

  it('carries the stored public name on a custom-named unfinished requeue row (TC-01c)', () => {
    const rows = buildImportRows(['claude-sonnet-x'], [mapped('sonnet', 0, 'claude-sonnet-x')], {})
    expect(toImportItems([{ ...rows[0], checked: true }])).toStrictEqual([
      { provider_model_name: 'claude-sonnet-x', model_name: 'sonnet' },
    ])
  })

  it('submits the seven declaration fields only where the row declares them', () => {
    expect(toImportItems([
      row({
        name: 'full',
        pubName: 'public-name',
        inMods: ['text', 'image'],
        ctxLength: 200000,
        maxOut: 8192,
        tools: 2,
        reasoning: 1,
        temp: 0.7,
      }),
    ])).toStrictEqual([{
      provider_model_name: 'full',
      model_name: 'public-name',
      output_modalities: ['text'],
      input_modalities: ['text', 'image'],
      context_length: 200000,
      max_output: 8192,
      supports_tools: 'no',
      supports_reasoning: 'yes',
      temperature: 0.7,
    }])
    // A row that declares nothing keeps all seven off the wire — model_name,
    // input_modalities, context_length, supports_tools, supports_reasoning,
    // temperature, max_output.
    expect(toImportItems([row({ name: 'bare' })])).toStrictEqual([
      { provider_model_name: 'bare', output_modalities: ['text'] },
    ])
  })

  it('submits the row axes as the per-item output declaration', () => {
    const rows: ImportRow[] = [
      row({ name: 'img', out: ['image'] }),
      row({ name: 'both', out: ['text', 'image'] }),
      row({ name: 'txt', out: ['text'] }),
    ]
    expect(toImportItems(rows).map((i) => i.output_modalities)).toEqual([['image'], ['text', 'image'], ['text']])
  })
})

describe('import modalities', () => {
  it('preselects image for known image-output families and text otherwise', () => {
    const rows = buildImportRows(['wan2.7-image-pro', 'qwen-vl-max', 'deepseek-v4'], [], {})
    expect(rows.map((r) => r.out)).toEqual([['image'], ['text'], ['text']])
  })

  it('matches vendor-namespaced catalogue ids on their last segment', () => {
    const rows = buildImportRows(['ByteDance/Seedream-4.0', 'black-forest-labs/FLUX.1-schnell', 'Qwen/Qwen3-235B'], [], {})
    expect(rows.map((r) => r.out)).toEqual([['image'], ['image'], ['text']])
  })

  it('keeps added rows on text: their modality is server-owned', () => {
    const rows = buildImportRows(['wan2.7-image'], [mapped('wan2.7-image', 0)], {})
    expect(rows[0].out).toEqual(['text'])
  })
})

describe('statusOf', () => {
  it('reads a row as unconfigured until a capability field is declared (TC-09)', () => {
    // Untouched.
    expect(statusOf(row())).toBe('unconfigured')
    // Only the public name changed.
    expect(statusOf(row({ pubName: 'alias' }))).toBe('unconfigured')
    // Only the output axes changed — either axis alone or the 'both' pair;
    // both have fully default-shaped answers server-side.
    expect(statusOf(row({ out: ['image'] }))).toBe('unconfigured')
    expect(statusOf(row({ out: ['text', 'image'] }))).toBe('unconfigured')
    // A heuristic preselection is exactly that axis change — still untouched
    // as far as the admin's knowledge goes.
    const heuristic = buildImportRows(['wan2.7-image-pro'], [], {})[0]
    expect(statusOf(heuristic)).toBe('unconfigured')
  })

  it('reads a row as configured once any capability field is declared (TC-09)', () => {
    expect(statusOf(row({ inMods: ['text'] }))).toBe('configured')
    expect(statusOf(row({ ctxLength: 128000 }))).toBe('configured')
    expect(statusOf(row({ maxOut: 8192 }))).toBe('configured')
    expect(statusOf(row({ temp: 0.7 }))).toBe('configured')
    expect(statusOf(row({ tools: 1 }))).toBe('configured')
    expect(statusOf(row({ reasoning: 2 }))).toBe('configured')
    // An added row is configured by definition: its capabilities live on the
    // stored model, not on the row.
    expect(statusOf(row({ added: true }))).toBe('configured')
  })
})
