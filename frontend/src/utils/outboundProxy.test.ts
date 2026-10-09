import { describe, expect, it } from 'vitest'
import {
  checkProxyUrl,
  outboundProxyFormFromSetting,
  outboundProxySavePlan,
  providerProxyEffective,
  providerProxySavePlan,
  proxyUrlIssueMessageKey,
} from './outboundProxy'

// The pure logic the settings card and the provider dialog share: the
// client-side syntax pre-check (the server re-validates and additionally
// refuses reserved-range addresses) and the GET→form→PUT projections.

describe('checkProxyUrl', () => {
  it('accepts the four transport schemes, socks5h as the alias of socks5', () => {
    for (const url of [
      'http://127.0.0.1:7890',
      'https://proxy.example.com:8443',
      'socks5://192.168.1.5:1080',
      'socks5h://192.168.1.5:1080',
      'http://user:secret@10.0.0.2:3128',
      'socks5://[::1]:1080',
    ]) {
      expect(checkProxyUrl(url), url).toEqual({ ok: true, url })
    }
  })

  it('trims surrounding whitespace before judging and returns the trimmed URL', () => {
    expect(checkProxyUrl('  socks5://192.168.1.5:1080  ')).toEqual({
      ok: true,
      url: 'socks5://192.168.1.5:1080',
    })
  })

  it('refuses the illegal shapes with their own issue', () => {
    expect(checkProxyUrl('')).toEqual({ ok: false, issue: 'empty' })
    expect(checkProxyUrl(null)).toEqual({ ok: false, issue: 'empty' })
    expect(checkProxyUrl('ftp://proxy.example.com:21')).toEqual({ ok: false, issue: 'scheme' })
    // A schemeless host:port string parses as a weird-scheme URL in the
    // browser's URL parser; the scheme check catches it either way.
    expect(checkProxyUrl('proxy.example.com:7890')).toEqual({ ok: false, issue: 'scheme' })
    // http/https are special schemes: the URL parser throws on a missing
    // host before the hostname check runs. socks5/socks5h are not, so they
    // PARSE with an empty host — that is the one route into noHost.
    expect(checkProxyUrl('http://')).toEqual({ ok: false, issue: 'unparseable' })
    expect(checkProxyUrl('http://:7890')).toEqual({ ok: false, issue: 'unparseable' })
    expect(checkProxyUrl('socks5://')).toEqual({ ok: false, issue: 'noHost' })
  })
})

describe('proxyUrlIssueMessageKey', () => {
  it('scopes the message key to the calling surface', () => {
    expect(proxyUrlIssueMessageKey('settings', 'scheme')).toBe('generalSettings.outboundProxy.proxyUrlScheme')
    expect(proxyUrlIssueMessageKey('provider', 'scheme')).toBe('providers.proxyUrlScheme')
    expect(proxyUrlIssueMessageKey('settings', 'empty')).toBe('generalSettings.outboundProxy.proxyUrlRequired')
    expect(proxyUrlIssueMessageKey('provider', 'empty')).toBe('providers.proxyUrlRequired')
  })
})

describe('outboundProxyFormFromSetting', () => {
  it('projects the committed row into the form', () => {
    expect(outboundProxyFormFromSetting({ mode: 'custom', url: 'socks5://10.0.0.9:1080' })).toEqual({
      mode: 'custom',
      url: 'socks5://10.0.0.9:1080',
    })
  })

  it('degrades an unknown mode (a server newer than this client) to off', () => {
    expect(outboundProxyFormFromSetting({ mode: 'quantum', url: 'x' })).toEqual({ mode: 'off', url: 'x' })
  })
})

describe('outboundProxySavePlan', () => {
  it('carries the CAS version and the trimmed URL on a custom save', () => {
    expect(outboundProxySavePlan({ mode: 'custom', url: '  socks5://10.0.0.9:1080 ' }, 7)).toEqual({
      ok: true,
      payload: { mode: 'custom', url: 'socks5://10.0.0.9:1080', version: 7 },
    })
  })

  it('saves off/system with an empty URL without demanding one', () => {
    expect(outboundProxySavePlan({ mode: 'off', url: '' }, 7)).toEqual({
      ok: true,
      payload: { mode: 'off', url: '', version: 7 },
    })
    expect(outboundProxySavePlan({ mode: 'system', url: '' }, 7)).toEqual({
      ok: true,
      payload: { mode: 'system', url: '', version: 7 },
    })
  })

  it('still syntax-checks a non-empty URL parked behind off/system', () => {
    expect(outboundProxySavePlan({ mode: 'off', url: 'ftp://p:21' }, 7)).toEqual({
      ok: false,
      issue: 'scheme',
    })
  })

  it('blocks a custom save without a URL', () => {
    expect(outboundProxySavePlan({ mode: 'custom', url: '' }, 7)).toEqual({ ok: false, issue: 'empty' })
  })
})

describe('providerProxySavePlan', () => {
  it('emits the inherit/off/custom pair for the PATCH', () => {
    expect(providerProxySavePlan('', '')).toEqual({ ok: true, payload: { proxy_mode: '', proxy_url: '' } })
    expect(providerProxySavePlan('off', '')).toEqual({ ok: true, payload: { proxy_mode: 'off', proxy_url: '' } })
    expect(providerProxySavePlan('custom', 'socks5://10.0.0.9:1080')).toEqual({
      ok: true,
      payload: { proxy_mode: 'custom', proxy_url: 'socks5://10.0.0.9:1080' },
    })
  })

  it('blocks custom without a URL and any illegal non-empty URL', () => {
    expect(providerProxySavePlan('custom', '')).toEqual({ ok: false, issue: 'empty' })
    expect(providerProxySavePlan('custom', 'ftp://p:21')).toEqual({ ok: false, issue: 'scheme' })
    expect(providerProxySavePlan('', 'socks5://')).toEqual({ ok: false, issue: 'noHost' })
  })
})

// The display rule behind the provider pages' warning icon: whether a
// provider's stance resolves to proxied once it meets the global setting —
// provider explicit beats global, inherit resolves against it, and unknown
// strings never invent a proxied claim.
describe('providerProxyEffective', () => {
  it('custom is proxied whatever the global says, off is direct whatever the global says', () => {
    expect(providerProxyEffective('custom', 'off')).toBe(true)
    expect(providerProxyEffective('custom', 'custom')).toBe(true)
    expect(providerProxyEffective('custom', 'system')).toBe(true)
    expect(providerProxyEffective('off', 'custom')).toBe(false)
    expect(providerProxyEffective('off', 'system')).toBe(false)
    expect(providerProxyEffective('off', 'off')).toBe(false)
  })

  it('inherit resolves against the global mode, system counting as proxied', () => {
    expect(providerProxyEffective('', 'custom')).toBe(true)
    expect(providerProxyEffective('', 'system')).toBe(true)
    expect(providerProxyEffective('', 'off')).toBe(false)
  })

  it('unknown strings normalize without inventing a stance: global to off, provider to inherit', () => {
    // A garbage global mode must not claim proxied (normalize-to-off); a
    // garbage provider stance reads as inherit, so it resolves against the
    // global — erring toward the warning rather than hiding it.
    expect(providerProxyEffective('', 'garbage')).toBe(false)
    expect(providerProxyEffective('garbage', 'custom')).toBe(true)
    expect(providerProxyEffective('garbage', 'off')).toBe(false)
  })
})
