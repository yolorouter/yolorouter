import { describe, expect, it } from 'vitest'
import { defaultConcisePrompt } from './concisePrompt'
import en from '../locales/en/costOptimization'
import zhCN from '../locales/zh-CN/costOptimization'

// Drives the real locale strings, so the assertions below fail if either
// example sentence is reworded into something that no longer joins cleanly.
const translate = (messages: Record<string, unknown>) => (key: string) =>
  String(messages[key.replace('costOptimization.', '')])

describe('defaultConcisePrompt', () => {
  it('separates the two English examples — without the space they collide mid-word', () => {
    const prompt = defaultConcisePrompt(translate(en), 'en')
    expect(prompt).toContain('detail. Prefer')
    expect(prompt).not.toContain('detail.Prefer')
  })

  it('joins the Chinese examples with no space — a full-width stop already separates them', () => {
    const prompt = defaultConcisePrompt(translate(zhCN), 'zh-CN')
    expect(prompt).toBe(zhCN.exampleConciseText + zhCN.exampleMinimalCodeText)
    expect(prompt).not.toContain('。 ')
  })

  it('carries both examples whole in either language', () => {
    for (const [messages, locale] of [[en, 'en'], [zhCN, 'zh-CN']] as const) {
      const prompt = defaultConcisePrompt(translate(messages), locale)
      expect(prompt).toContain(messages.exampleConciseText)
      expect(prompt).toContain(messages.exampleMinimalCodeText)
    }
  })
})
