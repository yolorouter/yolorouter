// Display projection for a model-catalogue fetch: turns the by-id endpoint's
// response into the reason an admin is shown, so the two causes that need
// opposite actions stop wearing the same message. A provider with no enabled,
// current key never reached the upstream at all — telling that admin to check
// the key and the base URL points them at the one thing that is not broken,
// when what they need is to test and enable a key. Kept as a pure function so
// the split is testable without mounting the dialog.
import type { ModelCatalogueResult } from '../api/providers'
import { isTestSuccess, testOutcomeLabel } from './testOutcomeDisplay'

export interface CatalogueFailure {
  /**
   * 'none' — the catalogue came back; 'no-usable-key' — no key was available
   * to authenticate with, so no fetch was attempted; 'fetch-failed' — a fetch
   * was attempted and did not produce a catalogue.
   *
   * Views only ask whether this is 'none': the two failures already differ in
   * the copy they carry, so nothing branches on which one it is. The values
   * are named apart anyway, so a test can state which failure it is asserting
   * and a view that needs to treat them differently can.
   */
  kind: 'none' | 'no-usable-key' | 'fetch-failed'
  /** Localized headline; empty when kind is 'none'. */
  title: string
  /** Localized failure category, when the response names one. */
  description: string
  /** Upstream diagnostic, shown verbatim; empty means there is none. */
  detail: string
}

// The nothing-to-report value, exported so a view can hold it as its initial
// and reset state instead of hand-building an equivalent object.
export const NO_CATALOGUE_FAILURE: CatalogueFailure = Object.freeze({
  kind: 'none',
  title: '',
  description: '',
  detail: '',
})

// Projects a catalogue response into the failure to show. Pass null when the
// request itself failed (network error, non-200): there is no response to
// classify, so it reports the generic fetch failure with no category. A
// response from a server that predates the split carries neither
// no_usable_key nor detail, and degrades to exactly the categorized fetch
// failure that was shown before.
export function catalogueFailure(
  t: (key: string) => string,
  result: ModelCatalogueResult | null,
): CatalogueFailure {
  if (result?.no_usable_key) {
    return { kind: 'no-usable-key', title: t('models.importNoUsableKey'), description: '', detail: '' }
  }
  const outcome = result?.outcome ?? null
  if (outcome !== null && isTestSuccess(outcome)) return NO_CATALOGUE_FAILURE
  return {
    kind: 'fetch-failed',
    title: t('models.importFetchFailed'),
    description: outcome === null ? '' : testOutcomeLabel(t, outcome),
    // Surrounding whitespace only decides whether an empty diagnostic renders
    // as an empty box; the message itself is never rewritten.
    detail: (result?.detail ?? '').trim(),
  }
}
