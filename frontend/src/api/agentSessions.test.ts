// frontend/src/api/agentSessions.test.ts
//
// The transcript client's cursor-query construction (TC-13): every param
// combination encodes under the handler's exact param names, and absent
// params stay absent — a defaulted call must not put an empty ?before= or
// ?limit= on the wire (the handler treats a present-but-garbage cursor as
// an empty page, so a stray empty param is not cosmetic). Also pins the
// URL shapes of both transcript endpoints against apiFetch, so the client
// and the router cannot drift apart silently.
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { apiFetch } from './client'
import { buildTranscriptQuery, getAgentSessionTranscript, getAgentSessionTranscriptSystem } from './agentSessions'

vi.mock('./client', () => ({ apiFetch: vi.fn() }))

const fetchMock = vi.mocked(apiFetch)

beforeEach(() => {
  fetchMock.mockResolvedValue({} as never)
})

afterEach(() => {
  vi.clearAllMocks()
})

describe('buildTranscriptQuery', () => {
  it.each([
    // A defaulted call: nothing on the wire at all.
    ['no params (the latest-page opening)', {}, ''],
    ['limit only', { limit: 20 }, 'limit=20'],
    ['before only', { before: 'b14' }, 'before=b14'],
    ['after only', { after: 'req-0009' }, 'after=req-0009'],
    ['after + limit (the anchor entry shape)', { after: 'req-0009', limit: 50 }, 'limit=50&after=req-0009'],
    // Undefined optionals are the same as absent — no empty params.
    ['undefined members', { limit: undefined, before: undefined, after: undefined }, ''],
    // An empty-string cursor is dropped like any other falsy filter value.
    ['empty-string cursor', { before: '' }, ''],
  ])('%s', (_name, params, expected) => {
    expect(buildTranscriptQuery(params).toString()).toBe(expected)
  })

  it('round-trips values that need URL encoding', () => {
    const sp = buildTranscriptQuery({ after: 'id with spaces/&' })
    expect(sp.get('after')).toBe('id with spaces/&')
    expect(sp.toString()).toContain(encodeURIComponent('id with spaces/&').replace(/%20/g, '+'))
  })
})

describe('getAgentSessionTranscript', () => {
  it('a defaulted call is a bare transcript URL — no trailing question mark', async () => {
    await getAgentSessionTranscript('sess-1')
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls[0][0]).toBe('/api/admin/agent-sessions/sess-1/transcript')
  })

  it('cursor params land on the wire under the handler names', async () => {
    await getAgentSessionTranscript('sess-1', { before: 'b14' })
    expect(fetchMock.mock.calls[0][0]).toBe('/api/admin/agent-sessions/sess-1/transcript?before=b14')
  })

  it('the session id path-segment is encoded', async () => {
    await getAgentSessionTranscript('sess/1', { limit: 5 })
    expect(fetchMock.mock.calls[0][0]).toBe('/api/admin/agent-sessions/sess%2F1/transcript?limit=5')
  })
})

describe('getAgentSessionTranscriptSystem', () => {
  it('a defaulted call is the bare raw-text endpoint — no trailing question mark', async () => {
    await getAgentSessionTranscriptSystem('sess-1')
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls[0][0]).toBe('/api/admin/agent-sessions/sess-1/transcript/system')
  })

  it('the per-request form carries the request param under the handler name', async () => {
    await getAgentSessionTranscriptSystem('sess-1', { request: 'req-0009' })
    expect(fetchMock.mock.calls[0][0]).toBe('/api/admin/agent-sessions/sess-1/transcript/system?request=req-0009')
  })

  it('an empty request value is dropped — the bare URL, the handler whole-timeline form', async () => {
    await getAgentSessionTranscriptSystem('sess-1', { request: '' })
    expect(fetchMock.mock.calls[0][0]).toBe('/api/admin/agent-sessions/sess-1/transcript/system')
  })
})
