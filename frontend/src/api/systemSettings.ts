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
