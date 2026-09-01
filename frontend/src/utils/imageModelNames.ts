// Heuristic guess of "is this upstream catalogue id an image-output model",
// used only to PRESELECT the modality column of the bulk-import dialog — the
// admin can always override, and a wrong guess costs nothing server-side (the
// declaration is per-row and editable before submit).
//
// Ids are matched on their LAST slash segment: aggregator catalogues use
// vendor-namespaced ids ("ByteDance/Seedream-4.0"), and the family spelling
// lives in that segment, not the namespace.

// The families themselves, as case-insensitive anchored patterns. Deliberately
// a curated list of well-known image-output spellings rather than a loose
// "contains image" test: vision-INPUT families (qwen-vl, gpt-4o) and other
// multimodal text models must not light up, and "image" appears in their
// marketing names far more often than in their ids.
const IMAGE_OUTPUT_FAMILIES: readonly RegExp[] = [
  /^wanx/, // Alibaba Wanx image studio line — every spelling is image output
           // (wanx-v1, wanx2.1-t2i-turbo, wanx2.1-imageedit)
  /^wan[\d.]*-image/, // wan2.7-image, wan2.2-image-pro (NOT wan2.x tts/live)
  /^qwen-image/, // qwen-image, qwen-image-edit (NOT qwen-vl, a vision input)
  /^dall-e/,
  /^gpt-image/, // gpt-image-1 (NOT gpt-4o, multimodal but text-output)
  /^seedream/, // Volcengine text-to-image
  /^jimeng/, // Volcengine Jimeng image generation
  /^flux/, // Black Forest Labs image family (FLUX.1-schnell)
  /^cogview/, // Zhipu image generation
  /^imagen/, // Google image generation
  /^stable-diffusion/,
  /^sd3/, // Stable Diffusion 3 shorthand
  /^sdxl/,
]

export function suggestsImageOutput(name: string): boolean {
  const lastSegment = name.trim().split('/').pop() ?? ''
  if (lastSegment === '') return false
  const lower = lastSegment.toLowerCase()
  return IMAGE_OUTPUT_FAMILIES.some((pattern) => pattern.test(lower))
}
