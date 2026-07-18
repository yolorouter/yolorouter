import { apiFetch } from './client'

export interface ProviderKey {
  id: number
  label: string
  key_prefix: string
  sort_order: number
  test_model: string
  management_status: number
  verification_status: number
  needs_reentry: boolean
  last_test_result: number | null
  last_test_model: string
  last_test_duration_ms: number | null
  last_tested_at: string | null
}

export interface Provider {
  id: number
  name: string
  provider_type: string
  base_url: string
  note: string
  management_status: number
  running_status: 'not_configured' | 'pending_test' | 'available' | 'partial' | 'unavailable'
  keys: ProviderKey[]
  created_at: string
}

export interface BatchTestResult {
  key_id: number
  label: string
  needs_reentry: boolean
  skipped: boolean
  outcome: number | null
  duration_ms: number
}

export interface CreateProviderInput {
  name: string
  base_url: string
  note?: string
  key_label: string
  key_plaintext: string
  test_model: string
  management_status?: number
}

export interface UpdateProviderInput {
  name: string
  base_url: string
  note?: string
}

export interface CreateKeyInput {
  label: string
  plaintext: string
  test_model: string
  management_status?: number
}

export interface UpdateKeyInput {
  label: string
  plaintext?: string
  test_model: string
  management_status?: number
}

export interface TestKeyResult {
  outcome: number
  duration_ms: number
}

export function listProviders(): Promise<{ list: Provider[] }> {
  return apiFetch('/api/admin/providers')
}

export function getProvider(id: number): Promise<Provider> {
  return apiFetch(`/api/admin/providers/${id}`)
}

export function createProvider(input: CreateProviderInput): Promise<Provider> {
  return apiFetch('/api/admin/providers', { method: 'POST', body: JSON.stringify(input) })
}

export function updateProvider(id: number, input: UpdateProviderInput): Promise<Provider> {
  return apiFetch(`/api/admin/providers/${id}`, { method: 'PATCH', body: JSON.stringify(input) })
}

export function setProviderStatus(id: number, enabled: boolean): Promise<void> {
  return apiFetch(`/api/admin/providers/${id}/status`, { method: 'PATCH', body: JSON.stringify({ enabled }) })
}

export function testKeyPreview(baseUrl: string, apiKey: string, model: string): Promise<TestKeyResult> {
  return apiFetch('/api/admin/providers/test-key', {
    method: 'POST',
    body: JSON.stringify({ base_url: baseUrl, api_key: apiKey, model }),
  })
}

export function createProviderKey(providerId: number, input: CreateKeyInput): Promise<ProviderKey> {
  return apiFetch(`/api/admin/providers/${providerId}/keys`, { method: 'POST', body: JSON.stringify(input) })
}

export function updateProviderKey(providerId: number, keyId: number, input: UpdateKeyInput): Promise<ProviderKey> {
  return apiFetch(`/api/admin/providers/${providerId}/keys/${keyId}`, { method: 'PATCH', body: JSON.stringify(input) })
}

export function reorderProviderKey(providerId: number, keyId: number, direction: 'up' | 'down'): Promise<void> {
  return apiFetch(`/api/admin/providers/${providerId}/keys/${keyId}/order`, {
    method: 'PATCH',
    body: JSON.stringify({ direction }),
  })
}

export function setProviderKeyStatus(providerId: number, keyId: number, enabled: boolean): Promise<void> {
  return apiFetch(`/api/admin/providers/${providerId}/keys/${keyId}/status`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled }),
  })
}

export function testProviderKey(providerId: number, keyId: number): Promise<ProviderKey> {
  return apiFetch(`/api/admin/providers/${providerId}/keys/${keyId}/test`, { method: 'POST' })
}

// design doc §7: batch test can legitimately exceed apiFetch's default
// 30s timeout — passes timeoutMs (Task 11 also modifies apiFetch itself to
// honor this override; see client.ts). A codex adversarial review round
// found an earlier version tried to work around this with an extra
// AbortController instead — that only added a SECOND, later abort signal
// on top of apiFetch's own hardcoded 30s internal timer, which still fired
// first regardless, so slow multi-key batches kept failing at 30s anyway.
export function testAllProviderKeys(
  providerId: number,
  enabledKeyCount: number,
): Promise<{ results: BatchTestResult[] }> {
  const timeoutMs = 60_000 + enabledKeyCount * 16_000
  return apiFetch<{ results: BatchTestResult[] }>(`/api/admin/providers/${providerId}/keys/test-all`, {
    method: 'POST',
    timeoutMs,
  })
}
