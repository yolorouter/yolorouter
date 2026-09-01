import { describe, expect, it } from 'vitest'
import { suggestsImageOutput } from './imageModelNames'

describe('suggestsImageOutput', () => {
  it('recognizes the known image-output families', () => {
    for (const name of [
      'wanx-v1',
      'wanx2.1-t2i-turbo',
      'wan2.2-image',
      'wan2.7-image-pro',
      'qwen-image',
      'qwen-image-edit',
      'dall-e-3',
      'gpt-image-1',
      'gpt-image-1-mini',
      'seedream-4.0',
      'jimeng-t2i-2.1',
      'flux-schnell',
      'FLUX.1-dev',
      'cogview-4',
      'imagen-4.0',
      'stable-diffusion-3.5',
      'sd3.5-large',
      'sdxl-base',
    ]) {
      expect(suggestsImageOutput(name), name).toBe(true)
    }
  })

  it('does not light up text-output or vision-INPUT families', () => {
    for (const name of [
      'qwen-vl-max',
      'qwen2.5-vl-72b-instruct',
      'gpt-4o',
      'gpt-4o-mini',
      'deepseek-v4',
      'Qwen/Qwen3-235B-A22B',
      'text-embedding-3-large',
      'wan2.2-tts-live',
      'gemini-2.5-flash',
      'claude-sonnet-5',
      'glm-4.7',
    ]) {
      expect(suggestsImageOutput(name), name).toBe(false)
    }
  })

  it('matches vendor-namespaced ids on their last segment, case-insensitively', () => {
    expect(suggestsImageOutput('ByteDance/Seedream-4.0')).toBe(true)
    expect(suggestsImageOutput('black-forest-labs/FLUX.1-schnell')).toBe(true)
    expect(suggestsImageOutput('OpenAI/GPT-Image-1')).toBe(true)
    // The namespace itself never decides: a non-image id under an image vendor
    // stays text.
    expect(suggestsImageOutput('black-forest-labs/fluid')).toBe(false)
  })

  it('handles blanks and slash-only ids without throwing', () => {
    expect(suggestsImageOutput('')).toBe(false)
    expect(suggestsImageOutput('   ')).toBe(false)
    expect(suggestsImageOutput('/')).toBe(false)
  })
})
