import { apiFetch } from './client'

export interface APIKey {
  id: number
  key_prefix: string
  owner_label: string
  remark: string
  status: number
  display_status: string
  expires_at: string | null
  rpm_limit: number | null
  tpm_limit: number | null
  concurrency_limit: number | null
  budget_limit_cents: number | null
  budget_spent_cents: number
  model_ids: number[]
  created_at: string
  updated_at: string
}

export interface APIKeyPage {
  total: number
  page: number
  page_size: number
  list: APIKey[]
}

export interface CreateAPIKeyInput {
  owner_label?: string
  remark?: string
  model_ids: number[]
  expires_at?: string
  rpm_limit?: number
  tpm_limit?: number
  concurrency_limit?: number
  budget_limit_cents?: number
}

export interface CreateAPIKeyResult {
  plaintext_key: string
  api_key: APIKey
}

// UpdateAPIKeyInput is a sparse PATCH. Numeric limits: undefined = leave
// unchanged; 0 = clear sentinel; positive = set. model_ids: undefined =
// unchanged; an array (including empty) replaces the whitelist. owner_label /
// remark: undefined = unchanged.
export interface UpdateAPIKeyInput {
  owner_label?: string
  remark?: string
  model_ids?: number[]
  expires_at?: string
  rpm_limit?: number
  tpm_limit?: number
  concurrency_limit?: number
  budget_limit_cents?: number
}

export function listAPIKeys(q: string, page: number, pageSize: number): Promise<APIKeyPage> {
  const params = new URLSearchParams({ q, page: String(page), page_size: String(pageSize) })
  return apiFetch(`/api/admin/api-keys?${params.toString()}`)
}

export function createAPIKey(input: CreateAPIKeyInput): Promise<CreateAPIKeyResult> {
  return apiFetch('/api/admin/api-keys', { method: 'POST', body: JSON.stringify(input) })
}

export function getAPIKey(id: number): Promise<APIKey> {
  return apiFetch(`/api/admin/api-keys/${id}`)
}

export function updateAPIKey(id: number, input: UpdateAPIKeyInput): Promise<APIKey> {
  return apiFetch(`/api/admin/api-keys/${id}`, { method: 'PATCH', body: JSON.stringify(input) })
}

export function revokeAPIKey(id: number): Promise<void> {
  return apiFetch(`/api/admin/api-keys/${id}/revoke`, { method: 'PATCH', body: JSON.stringify({}) })
}
