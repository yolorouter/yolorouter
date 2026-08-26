// Built-in catalogue of mainstream model IDs, shown as pick-and-batch-add
// groups in the create-model dialog. Selecting entries and confirming creates
// each as an outward model name in one request; names that already exist are
// skipped server-side. Everything here is public, non-secret configuration.
//
// Each id is the exact string a client sends in the `model` field against the
// vendor's OpenAI-compatible endpoint (e.g. "gpt-5.6"). Model IDs rotate
// quickly — this list is a convenience prefill, not a source of truth; an
// admin can always type a name by hand instead.
//
// The vendor display name is localized via the i18n key
// `models.presetVendor_<vendorId>` so each group heading reads in the admin's
// language. Model ids are not translated.

export interface ModelPresetGroup {
  // vendorId is a stable slug: the group key and the i18n heading lookup.
  vendorId: string
  models: string[]
}

export const MODEL_PRESET_GROUPS: ModelPresetGroup[] = [
  { vendorId: 'openai', models: ['gpt-5.6', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna'] },
  { vendorId: 'anthropic', models: ['claude-fable-5', 'claude-opus-4-8', 'claude-sonnet-5', 'claude-haiku-4-5'] },
  { vendorId: 'google', models: ['gemini-3.5-flash', 'gemini-3.5-flash-lite', 'gemini-2.5-pro', 'gemini-2.5-flash'] },
  { vendorId: 'deepseek', models: ['deepseek-v4-flash', 'deepseek-v4-pro'] },
  { vendorId: 'qwen', models: ['qwen-max', 'qwen-plus', 'qwen-flash', 'qwen-vl-max'] },
  { vendorId: 'kimi', models: ['kimi-k3', 'kimi-k2.7-code', 'kimi-k2.6'] },
  { vendorId: 'zhipu', models: ['glm-5.3', 'glm-5-turbo', 'glm-5.2', 'glm-5.1', 'glm-5', 'glm-4.7', 'glm-4.6'] },
  { vendorId: 'minimax', models: ['MiniMax-M3', 'MiniMax-M2.7', 'MiniMax-M2'] },
]
