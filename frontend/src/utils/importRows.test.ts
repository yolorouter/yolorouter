import { describe, expect, it } from 'vitest'
import { buildImportRows, chunkByCap, isTransportableModelName, normalizeCatalogNames, toImportItems, type ImportRow } from './importRows'

const price = (input: number, output: number, source: 'history' | 'seed') => ({
  input_price: input,
  output_price: output,
  cache_write_price: null,
  cache_read_price: null,
  source,
  catalog_updated_at: '',
})

// An existing mapping as the candidates endpoint reports it: settled rows hold
// a verdict (verification_status 1/2), unfinished ones are still Untested (0).
const mapped = (name: string, verification: number) => ({ model_name: name, verification_status: verification })

describe('buildImportRows', () => {
  it('checks price-suggested rows by default and prefills their prices', () => {
    const rows = buildImportRows(['deepseek-v4', 'obscure-embedding'], [], {
      'deepseek-v4': price(2, 8, 'seed'),
    })
    expect(rows).toHaveLength(2)
    const [priced, unpriced] = rows
    expect(priced).toMatchObject({ name: 'deepseek-v4', added: false, checked: true, priceSource: 'seed', inputPrice: 2, outputPrice: 8 })
    expect(unpriced).toMatchObject({ name: 'obscure-embedding', checked: false, priceSource: '', inputPrice: null, outputPrice: null })
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
    const items = Array.from({ length: 5 }, (_, i) => ({
      provider_model_name: `m-${i}`,
      input_price: 0,
      output_price: 0,
      cache_write_price: null,
      cache_read_price: null,
    }))
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
    // larger than one request must have its names batched too, or every row
    // past the cap silently loses its suggestion.
    expect(chunkByCap(['a', 'b', 'c'], 2)).toEqual([['a', 'b'], ['c']])
  })
})

describe('toImportItems', () => {
  it('submits checked new rows and checked unfinished rows, defaulting blank prices to zero', () => {
    const rows: ImportRow[] = [
      { name: 'a', added: false, unfinished: false, checked: true, modality: 'text', priceSource: 'seed', inputPrice: 2, outputPrice: 8, cacheWritePrice: 0.5, cacheReadPrice: null },
      { name: 'b', added: false, unfinished: false, checked: true, modality: 'text', priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null },
      { name: 'c', added: false, unfinished: false, checked: false, modality: 'text', priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null },
      { name: 'd', added: true, unfinished: false, checked: false, modality: 'text', priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null },
      // A checked unfinished mapping goes in the payload: the server skips it
      // as existing but hands back its candidate id for requeueing.
      { name: 'e', added: true, unfinished: true, checked: true, modality: 'text', priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null },
    ]
    expect(toImportItems(rows)).toEqual([
      { provider_model_name: 'a', output_modalities: ['text'], input_price: 2, output_price: 8, cache_write_price: 0.5, cache_read_price: null },
      { provider_model_name: 'b', output_modalities: ['text'], input_price: 0, output_price: 0, cache_write_price: null, cache_read_price: null },
      // An added row carries NO declaration: the server reads a present
      // declaration on an existing model as a claim that must match the
      // stored one, and the requeue path must not submit a claim (the
      // modality select is inert on added rows).
      { provider_model_name: 'e', input_price: 0, output_price: 0, cache_write_price: null, cache_read_price: null },
    ])
  })
})

describe('import modalities', () => {
  it('preselects image for known image-output families and text otherwise', () => {
    const rows = buildImportRows(['wan2.7-image-pro', 'qwen-vl-max', 'deepseek-v4'], [], {})
    expect(rows.map((r) => r.modality)).toEqual(['image', 'text', 'text'])
  })

  it('matches vendor-namespaced catalogue ids on their last segment', () => {
    const rows = buildImportRows(['ByteDance/Seedream-4.0', 'black-forest-labs/FLUX.1-schnell', 'Qwen/Qwen3-235B'], [], {})
    expect(rows.map((r) => r.modality)).toEqual(['image', 'image', 'text'])
  })

  it('submits the row modality as the per-item declaration', () => {
    const rows: ImportRow[] = [
      { name: 'img', added: false, unfinished: false, checked: true, modality: 'image', priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null },
      { name: 'both', added: false, unfinished: false, checked: true, modality: 'both', priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null },
      { name: 'txt', added: false, unfinished: false, checked: true, modality: 'text', priceSource: '', inputPrice: null, outputPrice: null, cacheWritePrice: null, cacheReadPrice: null },
    ]
    expect(toImportItems(rows).map((i) => i.output_modalities)).toEqual([['image'], ['text', 'image'], ['text']])
  })

  it('keeps added rows on text: their modality is server-owned', () => {
    const rows = buildImportRows(['wan2.7-image'], [mapped('wan2.7-image', 0)], {})
    expect(rows[0].modality).toBe('text')
  })
})
