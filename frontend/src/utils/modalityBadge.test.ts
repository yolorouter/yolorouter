import { describe, expect, it } from 'vitest'
import { modalityBadge } from './modalityBadge'

// The badge rule: text-only carries no badge (the default modality badging
// every row says nothing), image-only reads image, both reads both — and the
// input is order-insensitive because the server's stored list has no order
// contract.
describe('modalityBadge', () => {
  it('returns null for text-only models', () => {
    expect(modalityBadge(['text'])).toBeNull()
    expect(modalityBadge([])).toBeNull()
  })

  it('badges image-only models as image', () => {
    expect(modalityBadge(['image'])).toEqual({ key: 'image' })
  })

  it('badges both models as both, regardless of order', () => {
    expect(modalityBadge(['text', 'image'])).toEqual({ key: 'both' })
    expect(modalityBadge(['image', 'text'])).toEqual({ key: 'both' })
  })

  it('ignores unknown modality ids rather than guessing a badge', () => {
    expect(modalityBadge(['audio'])).toBeNull()
    expect(modalityBadge(['text', 'audio'])).toBeNull()
  })
})
