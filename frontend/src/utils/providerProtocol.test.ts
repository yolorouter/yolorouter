import { describe, expect, it } from 'vitest'
import { protocolLabel } from './providerProtocol'

// The identity t makes the exact i18n key visible, so a label that stopped
// resolving is a failed assertion rather than an empty string nobody notices.
const t = (key: string) => key

describe('protocolLabel', () => {
  it('names a known protocol through i18n', () => {
    expect(protocolLabel(t, 'anthropic')).toBe('providers.protocol_anthropic')
  })

  it('falls back to the raw id for a protocol with no label', () => {
    expect(protocolLabel(t, 'grok')).toBe('grok')
  })
})
