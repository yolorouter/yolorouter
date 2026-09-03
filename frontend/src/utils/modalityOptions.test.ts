import { describe, expect, it } from 'vitest'
import { enforceVideoExclusivity, outputModalityOptions } from './modalityOptions'

// The picker rule pair: the option list mirrors the server's stored
// vocabulary, and the exclusivity watch keeps a video pick from coexisting
// with anything else — the server defines video as an exclusive modality,
// so the picker collapses the declaration the moment video joins it rather
// than letting a mixed list reach the server to be refused there.
describe('outputModalityOptions', () => {
  it('lists exactly the server vocabulary, labels via i18n keys', () => {
    const t = (key: string) => `t(${key})`
    const options = outputModalityOptions(t)
    expect(options.map((o) => o.value)).toEqual(['text', 'image', 'video'])
    expect(options.map((o) => o.label)).toEqual([
      't(models.outputModalityText)',
      't(models.outputModalityImage)',
      't(models.outputModalityVideo)',
    ])
  })
})

describe('enforceVideoExclusivity', () => {
  it('leaves a declaration without video untouched', () => {
    expect(enforceVideoExclusivity(['text'])).toEqual(['text'])
    expect(enforceVideoExclusivity(['text', 'image'])).toEqual(['text', 'image'])
    expect(enforceVideoExclusivity([])).toEqual([])
  })

  it('collapses video-plus-anything to video alone', () => {
    expect(enforceVideoExclusivity(['text', 'video'])).toEqual(['video'])
    expect(enforceVideoExclusivity(['video', 'image', 'text'])).toEqual(['video'])
  })

  it('keeps a video-only declaration as-is', () => {
    expect(enforceVideoExclusivity(['video'])).toEqual(['video'])
  })
})
