// frontend/src/utils/contextLengthPresets.ts
// The context-length quick picks shared by every dialog that edits the
// model-level context declaration (model edit, bulk import's detail pane).
// Values follow each tier's canonical real-world figure — powers of two for
// the binary tiers (32K/128K/1M, the Llama/Qwen/Gemini lineage) and
// Anthropic's exact 200,000 for 200K. The input beside the presets stays
// freely editable for anything else.
export const CONTEXT_LENGTH_PRESETS: { label: string; value: number }[] = [
  { label: '32K', value: 32768 },
  { label: '128K', value: 131072 },
  { label: '200K', value: 200000 },
  { label: '1M', value: 1048576 },
]
