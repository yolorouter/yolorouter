import { describe, expect, it } from 'vitest'
import type { ProviderKey } from '../api/providers'
import { deleteConfirmUnlocked, deleteLeavesProviderUnusable } from './providerStatusDisplay'

// Only the three fields isKeyUsable reads matter; the rest are inert
// scaffolding so the fixtures satisfy the ProviderKey shape.
function makeKey(id: number, usable: boolean): ProviderKey {
  return {
    id,
    label: `key-${id}`,
    key_prefix: 'sk-',
    sort_order: id,
    test_model: 'm',
    management_status: usable ? 1 : 2,
    verification_status: usable ? 1 : 0,
    needs_reentry: false,
    last_test_result: null,
    last_test_model: '',
    last_test_duration_ms: null,
    last_tested_at: null,
  }
}

describe('deleteLeavesProviderUnusable', () => {
  it('warns when deleting the last key at all, usable or not', () => {
    expect(deleteLeavesProviderUnusable([makeKey(1, true)], 1)).toBe(true)
    expect(deleteLeavesProviderUnusable([makeKey(1, false)], 1)).toBe(true)
  })

  it('warns when deleting the last usable key while unusable ones remain', () => {
    expect(deleteLeavesProviderUnusable([makeKey(1, true), makeKey(2, false)], 1)).toBe(true)
  })

  it('stays quiet when another usable key remains', () => {
    expect(deleteLeavesProviderUnusable([makeKey(1, true), makeKey(2, true)], 1)).toBe(false)
  })

  it('stays quiet when deleting an unusable key beside a usable one', () => {
    expect(deleteLeavesProviderUnusable([makeKey(1, false), makeKey(2, true)], 1)).toBe(false)
  })

  it('stays quiet when deleting one of several already-unusable keys', () => {
    expect(deleteLeavesProviderUnusable([makeKey(1, false), makeKey(2, false)], 1)).toBe(false)
  })

  it('reports false for an id not in the list', () => {
    expect(deleteLeavesProviderUnusable([makeKey(1, true)], 99)).toBe(false)
  })
})

describe('deleteConfirmUnlocked', () => {
  it('unlocks only on the exact name', () => {
    expect(deleteConfirmUnlocked('openai', 'openai')).toBe(true)
  })

  it('stays locked on empty and partial input', () => {
    expect(deleteConfirmUnlocked('', 'openai')).toBe(false)
    expect(deleteConfirmUnlocked('open', 'openai')).toBe(false)
  })

  it('does not forgive case or whitespace differences', () => {
    expect(deleteConfirmUnlocked('OpenAI', 'openai')).toBe(false)
    expect(deleteConfirmUnlocked(' openai', 'openai')).toBe(false)
    expect(deleteConfirmUnlocked('openai ', 'openai')).toBe(false)
  })

  it('never unlocks against an empty provider name', () => {
    expect(deleteConfirmUnlocked('', '')).toBe(false)
  })
})
