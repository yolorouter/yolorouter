import { describe, expect, it } from 'vitest'
import { catalogueFailure } from './catalogueFailure'
import type { ModelCatalogueResult } from '../api/providers'

// The identity t makes the exact i18n keys visible, so a reason that silently
// stopped naming itself is a failed assertion rather than an empty string
// nobody notices.
const t = (key: string) => key

function response(overrides: Partial<ModelCatalogueResult> = {}): ModelCatalogueResult {
  return { models: [], outcome: 0, detail: '', ...overrides }
}

describe('catalogueFailure', () => {
  it('reports nothing when the catalogue came back', () => {
    expect(catalogueFailure(t, response({ models: ['gpt-4o-mini'], outcome: 0 }))).toEqual({
      kind: 'none',
      title: '',
      description: '',
      detail: '',
    })
  })

  it('names the missing key, and never the credential or the address, when no key was usable', () => {
    const failure = catalogueFailure(t, response({ outcome: null, no_usable_key: true }))

    expect(failure).toEqual({
      kind: 'no-usable-key',
      title: 'models.importNoUsableKey',
      description: '',
      detail: '',
    })
    expect(failure.title).not.toBe('models.importFetchFailed')
  })

  it('keeps naming the missing key even if a server also sends an outcome', () => {
    // The flag is the authority: an outcome alongside it would otherwise
    // reinstate the very message this split exists to stop showing.
    const failure = catalogueFailure(t, response({ outcome: 1, no_usable_key: true }))
    expect(failure.kind).toBe('no-usable-key')
  })

  it('categorizes a real refusal and carries the upstream words verbatim', () => {
    expect(
      catalogueFailure(t, response({ outcome: 7, detail: '  HTTP 400: unsupported endpoint  ' })),
    ).toEqual({
      kind: 'fetch-failed',
      title: 'models.importFetchFailed',
      description: 'providers.outcomeUpstreamError',
      detail: 'HTTP 400: unsupported endpoint',
    })
  })

  it('degrades to the categorized failure for a response that predates the split', () => {
    // No no_usable_key, no detail — the shape an older server answers with.
    expect(catalogueFailure(t, { models: [], outcome: 1 } as ModelCatalogueResult)).toEqual({
      kind: 'fetch-failed',
      title: 'models.importFetchFailed',
      description: 'providers.outcomeAuthFailed',
      detail: '',
    })
    expect(catalogueFailure(t, { models: ['a'], outcome: 0 } as ModelCatalogueResult).kind).toBe('none')
  })

  it('falls back to an uncategorized failure when the request itself failed', () => {
    expect(catalogueFailure(t, null)).toEqual({
      kind: 'fetch-failed',
      title: 'models.importFetchFailed',
      description: '',
      detail: '',
    })
  })
})
