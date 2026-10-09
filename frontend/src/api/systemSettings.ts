// frontend/src/api/systemSettings.ts
//
// API client for the global custom-system-prompt setting backing the
// Cost Optimization page. The GET returns the authoritative committed
// row (DB read, cache bypass) so the admin never sees a stale value;
// the PUT uses the returned version as an optimistic-lock token — a
// concurrent edit surfaces as errcode 11012 (HTTP 409), which the page
// treats as "reload and retry".

import { apiFetch } from './client'

export interface CustomSystemPromptSetting {
  enabled: boolean
  text: string
  version: number
}

export function getCustomSystemPrompt(): Promise<CustomSystemPromptSetting> {
  return apiFetch('/api/admin/system-settings/custom-system-prompt')
}

export function updateCustomSystemPrompt(payload: {
  enabled: boolean
  text: string
  version: number
}): Promise<CustomSystemPromptSetting> {
  return apiFetch('/api/admin/system-settings/custom-system-prompt', {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

// Global input-compression switch on the same Cost Optimization page. Same
// authoritative-read + CAS contract as the CSP endpoints above; a concurrent
// edit surfaces as errcode 11014 (HTTP 409), distinct from 11012 so the page
// can retry the right setting.
export interface InputCompressionSetting {
  enabled: boolean
  version: number
}

export function getInputCompression(): Promise<InputCompressionSetting> {
  return apiFetch('/api/admin/system-settings/input-compression')
}

export function updateInputCompression(payload: {
  enabled: boolean
  version: number
}): Promise<InputCompressionSetting> {
  return apiFetch('/api/admin/system-settings/input-compression', {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

// Global vision-fallback configuration: which model describes images for
// text-only targets (empty = feature off) and the describe prompt (empty =
// built-in default). Same authoritative-read + CAS contract; a concurrent
// edit surfaces as errcode 11017 (HTTP 409).
export interface VisionFallbackSetting {
  model: string
  prompt: string
  version: number
}

export function getVisionFallback(): Promise<VisionFallbackSetting> {
  return apiFetch('/api/admin/system-settings/vision-fallback')
}

export function updateVisionFallback(payload: {
  model: string
  prompt: string
  version: number
}): Promise<VisionFallbackSetting> {
  return apiFetch('/api/admin/system-settings/vision-fallback', {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

// Key auto recovery: the background loop that periodically re-tests provider
// keys removed from rotation for failed verification and puts passing ones
// straight back. Same authoritative-read + CAS contract as the settings
// above; a concurrent edit surfaces as errcode 11019 (HTTP 409).
export interface KeyAutoRecoverySetting {
  enabled: boolean
  interval_minutes: number
  version: number
}

export function getKeyAutoRecovery(): Promise<KeyAutoRecoverySetting> {
  return apiFetch('/api/admin/system-settings/key-auto-recovery')
}

export function updateKeyAutoRecovery(payload: {
  enabled: boolean
  interval_minutes: number
  version: number
}): Promise<KeyAutoRecoverySetting> {
  return apiFetch('/api/admin/system-settings/key-auto-recovery', {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

// Request log retention: how many days of request logs (rows + bodies +
// stream files) the background retention loop keeps. 0 means keep forever
// (the seeded default). Same authoritative-read + CAS contract as the
// settings above; a concurrent edit surfaces as errcode 11021 (HTTP 409).
export interface RequestLogRetentionSetting {
  retention_days: number
  version: number
}

export function getRequestLogRetention(): Promise<RequestLogRetentionSetting> {
  return apiFetch('/api/admin/system-settings/request-log-retention')
}

export function updateRequestLogRetention(payload: {
  retention_days: number
  version: number
}): Promise<RequestLogRetentionSetting> {
  return apiFetch('/api/admin/system-settings/request-log-retention', {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

// Admin audit retention: how many days of admin audit-log rows the
// background retention loop keeps. 0 means keep forever; the seeded default
// is 180 (unlike the request-log setting's 0 — an unattended audit trail
// slowly becomes a liability nobody reads). Same authoritative-read + CAS
// contract as the settings above; a concurrent edit surfaces as errcode
// 11023 (HTTP 409).
export interface AdminAuditRetentionSetting {
  retention_days: number
  version: number
}

export function getAdminAuditRetention(): Promise<AdminAuditRetentionSetting> {
  return apiFetch('/api/admin/system-settings/admin-audit-retention')
}

export function updateAdminAuditRetention(payload: {
  retention_days: number
  version: number
}): Promise<AdminAuditRetentionSetting> {
  return apiFetch('/api/admin/system-settings/admin-audit-retention', {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

// Outbound proxy: the global default for how upstream traffic leaves this
// instance — off (direct, the seeded default), system (read the process
// environment's HTTP_PROXY/HTTPS_PROXY/NO_PROXY), or custom (proxy through
// the stored URL). Per-provider overrides live on the provider rows and
// resolve against this global at request time. Same authoritative-read +
// CAS contract as the settings above; a concurrent edit surfaces as errcode
// 11025 (HTTP 409).
export interface OutboundProxySetting {
  mode: 'off' | 'system' | 'custom'
  url: string
  version: number
}

export function getOutboundProxy(): Promise<OutboundProxySetting> {
  return apiFetch('/api/admin/system-settings/outbound-proxy')
}

export function updateOutboundProxy(payload: {
  mode: string
  url: string
  version: number
}): Promise<OutboundProxySetting> {
  return apiFetch('/api/admin/system-settings/outbound-proxy', {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}
