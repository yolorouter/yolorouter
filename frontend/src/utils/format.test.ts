import { describe, expect, it } from 'vitest'
import { ccsProfileName } from './format'

describe('ccsProfileName', () => {
  it('brands the identity with the YoloRouter prefix', () => {
    expect(ccsProfileName('claude-sonnet-4')).toBe('YoloRouter - claude-sonnet-4')
    expect(ccsProfileName('alice (#42)')).toBe('YoloRouter - alice (#42)')
  })

  it('falls back to the brand-only name when the identity is missing', () => {
    expect(ccsProfileName('')).toBe('YoloRouter')
    expect(ccsProfileName(undefined)).toBe('YoloRouter')
  })
})
