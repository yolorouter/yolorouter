import { apiFetch } from './client'

export interface ModelCandidate {
  id: number
  provider_id: number
  provider_name: string
  provider_model_name: string
  input_price: number
  output_price: number
  cache_write_price: number | null
  cache_read_price: number | null
  max_output: number
  supports_streaming: boolean
  supports_function_calling: boolean
  management_status: number
  sort_order: number
  verification_status: number
  routable: boolean
  last_test_result: number | null
  last_test_duration_ms: number | null
  last_tested_at: string | null
}

export interface Model {
  id: number
  name: string
  management_status: number
  running_status: string
  candidates: ModelCandidate[]
  created_at: string
}

export interface CreateCandidateInput {
  provider_id: number
  provider_model_name: string
  input_price: number
  output_price: number
  cache_write_price?: number
  cache_read_price?: number
  max_output: number
  management_status?: number
}

export interface UpdateCandidateInput {
  provider_model_name: string
  input_price: number
  output_price: number
  cache_write_price?: number
  cache_read_price?: number
  max_output: number
}

export interface TestMappingResult {
  outcome: number
  duration_ms: number
}

export function listModels(): Promise<{ list: Model[] }> {
  return apiFetch('/api/admin/models')
}

export function createModel(name: string): Promise<Model> {
  return apiFetch('/api/admin/models', { method: 'POST', body: JSON.stringify({ name }) })
}

export function getModel(id: number): Promise<Model> {
  return apiFetch(`/api/admin/models/${id}`)
}

export function updateModel(id: number, name: string): Promise<Model> {
  return apiFetch(`/api/admin/models/${id}`, { method: 'PATCH', body: JSON.stringify({ name }) })
}

export function setModelStatus(id: number, enabled: boolean): Promise<void> {
  return apiFetch(`/api/admin/models/${id}/status`, { method: 'PATCH', body: JSON.stringify({ enabled }) })
}

export function testCandidateMapping(
  modelId: number,
  providerId: number,
  providerModelName: string,
  testType: 'basic' | 'streaming' | 'function_calling',
): Promise<TestMappingResult> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/test-mapping`, {
    method: 'POST',
    body: JSON.stringify({ provider_id: providerId, provider_model_name: providerModelName, test_type: testType }),
  })
}

export function createCandidate(modelId: number, input: CreateCandidateInput): Promise<ModelCandidate> {
  return apiFetch(`/api/admin/models/${modelId}/candidates`, { method: 'POST', body: JSON.stringify(input) })
}

export function updateCandidate(modelId: number, candidateId: number, input: UpdateCandidateInput): Promise<ModelCandidate> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/${candidateId}`, { method: 'PATCH', body: JSON.stringify(input) })
}

export function reorderCandidate(modelId: number, candidateId: number, direction: 'up' | 'down'): Promise<void> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/${candidateId}/order`, {
    method: 'PATCH',
    body: JSON.stringify({ direction }),
  })
}

export function setCandidateStatus(modelId: number, candidateId: number, enabled: boolean): Promise<void> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/${candidateId}/status`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled }),
  })
}

export function testCandidate(
  modelId: number,
  candidateId: number,
  testType: 'basic' | 'streaming' | 'function_calling',
): Promise<ModelCandidate> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/${candidateId}/test`, {
    method: 'POST',
    body: JSON.stringify({ test_type: testType }),
  })
}

export function deleteCandidate(modelId: number, candidateId: number): Promise<void> {
  return apiFetch(`/api/admin/models/${modelId}/candidates/${candidateId}`, { method: 'DELETE' })
}
