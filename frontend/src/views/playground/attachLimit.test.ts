// Pins the attach guard's mirror of the backend's per-image cap. The
// backend refuses one attachment whose data URL exceeds 8 MiB of characters
// (the whole URL, prefix included) with a 400 before the stream starts; the
// page refuses the file locally instead. These tests pin the boundary of
// that mirror from the encoding side: every file the local guard accepts
// must produce a data URL the backend accepts, and the first file the guard
// refuses must be the first whose URL no longer fits. The boundary is pinned
// twice — once against the module's own model (dataURLChars), and once
// against the REAL readAsDataURL prefix of every mime the attach input
// accepts (parsed out of PlaygroundPage.vue so the accept list cannot drift
// without a test going red). Revert the guard constant to a plain "6 MiB"
// and the boundary pins go red; shrink the model's prefix below the longest
// accepted mime's and the bound pin goes red.
import { describe, expect, it } from 'vitest'

import {
  MAX_IMAGE_DATA_URL_CHARS,
  MAX_IMAGE_DATA_URL_PREFIX_CHARS,
  dataURLChars,
  maxAttachableImageBytes,
} from './attachLimit'

// The page's own source, read as text through vite's ?raw query (same
// source-reading pattern as src/legacyEntryCleanup.test.ts) — the accept
// list below is parsed out of it, so this test tracks what the input
// actually accepts, not a copy of it.
import playgroundPageSource from './PlaygroundPage.vue?raw'

// The real prefix readAsDataURL emits for a mime: "data:<mime>;base64,".
function realPrefix(mime: string): string {
  return `data:${mime};base64,`
}

const acceptMatch = playgroundPageSource.match(/accept="([^"]+)"/)
const acceptedMimes = (acceptMatch?.[1] ?? '')
  .split(',')
  .map((mime) => mime.trim())
  .filter(Boolean)

describe('attachLimit mirrors the backend per-image data URL cap', () => {
  it('every file the local guard accepts produces a data URL within the cap', () => {
    expect(dataURLChars(maxAttachableImageBytes)).toBeLessThanOrEqual(MAX_IMAGE_DATA_URL_CHARS)
  })

  it('the first file the local guard refuses is the first whose data URL exceeds the cap', () => {
    expect(dataURLChars(maxAttachableImageBytes + 1)).toBeGreaterThan(MAX_IMAGE_DATA_URL_CHARS)
  })

  it('a 6 MiB file — the size the old "> 6 MiB" guard waved through — is over the cap once encoded', () => {
    // The regression this module closes: 6 MiB of image is at least
    // 8'388'630 characters of data URL (22-char png/gif prefix +
    // 4*ceil(6291456/3); jpeg/webp one more at 8'388'631), already past the
    // backend's 8'388'608, so the old local cap let a run start that the
    // endpoint then refused mid-run with a 400.
    expect(dataURLChars(6 * 1024 * 1024)).toBeGreaterThan(MAX_IMAGE_DATA_URL_CHARS)
  })

  it('the real prefixes are 22 characters for png/gif and 23 for jpeg/webp — not uniform', () => {
    // attachLimit.ts's comment states these exact lengths; pin them so the
    // comment cannot drift from reality unnoticed.
    expect(realPrefix('image/png')).toHaveLength(22)
    expect(realPrefix('image/gif')).toHaveLength(22)
    expect(realPrefix('image/jpeg')).toHaveLength(23)
    expect(realPrefix('image/webp')).toHaveLength(23)
  })

  it("the model's prefix bounds the real prefix of every mime the attach input accepts", () => {
    // The accept list is the source of truth for which mimes can arrive.
    // Pin its exact contents so a parse failure or a list change cannot
    // make this test vacuously green — adding a mime means widening this
    // pin AND re-checking the model covers the new prefix.
    expect(acceptedMimes).toEqual(['image/png', 'image/jpeg', 'image/webp', 'image/gif'])
    for (const mime of acceptedMimes) {
      expect(MAX_IMAGE_DATA_URL_PREFIX_CHARS).toBeGreaterThanOrEqual(realPrefix(mime).length)
    }
  })

  it('at the guard boundary every accepted mime still fits the cap; one byte more breaks it for all of them', () => {
    // The guarantee sentence, checked against real prefixes: at
    // maxAttachableImageBytes every accepted mime's URL is within the cap
    // (jpeg/webp, the longest, land at 8'388'607 ≤ 8'388'608), and at
    // maxAttachableImageBytes + 1 every accepted mime's URL is over it.
    for (const mime of acceptedMimes) {
      const atMax = realPrefix(mime).length + 4 * Math.ceil(maxAttachableImageBytes / 3)
      const onePast = realPrefix(mime).length + 4 * Math.ceil((maxAttachableImageBytes + 1) / 3)
      expect(atMax).toBeLessThanOrEqual(MAX_IMAGE_DATA_URL_CHARS)
      expect(onePast).toBeGreaterThan(MAX_IMAGE_DATA_URL_CHARS)
    }
  })
})
