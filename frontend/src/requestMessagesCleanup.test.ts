// Source hygiene gate for the request message page removal: the single-
// request conversation view (/request-messages/:requestId) was retired with
// the session replay rework, and these assertions pin that the removal
// stays complete — no leftover page file, no dangling references in
// non-test sources, and no orphaned requestMessages namespace in either
// locale tree — while the four message keys the surviving bubble renderer
// (and the replay surface built on it) still uses must NOT be swept away
// with the page: they now live under the agentSessions domain.
//
// The sources are read through vite's import.meta.glob with a ?raw query —
// every non-test .ts/.vue file under src/ as its own text, no fs walking
// (same shape as the earlier modal-removal hygiene gate).
import { describe, expect, it } from 'vitest'

import en from './locales/en'
import zhCN from './locales/zh-CN'

// Keyed by path ('./layouts/DefaultLayout.vue', ...); values are raw source.
const sources = import.meta.glob<string>('./**/*.{ts,vue}', {
  query: '?raw',
  import: 'default',
  eager: true,
})

const REMOVED_PAGE = 'RequestMessagePage'
const REMOVED_PATH = 'request-messages'

// Test files legitimately mention the removed names (this file does); the
// shipped sources must not.
function shippedSources(): [path: string, text: string][] {
  return Object.entries(sources).filter(([path]) => !path.endsWith('.test.ts'))
}

describe('request message page removal stays complete', () => {
  it('no removed-page view file remains anywhere under src', () => {
    const leftovers = Object.keys(sources).filter((path) => path.endsWith(`/${REMOVED_PAGE}.vue`))
    expect(leftovers, 'files named after the removed page').toEqual([])
  })

  it('no non-test source references the removed page or its URL segment', () => {
    const offenders = shippedSources()
      .filter(([, text]) => text.includes(REMOVED_PAGE) || text.includes(REMOVED_PATH))
      .map(([path]) => path)
    expect(offenders, 'sources still mentioning the removed page').toEqual([])
  })

  it('the requestMessages namespace is gone from both locale trees', () => {
    // The page's own copy went with the page; the four still-used keys
    // migrated to agentSessions (asserted below), so no trace may remain
    // under the old namespace — in either language.
    expect(en).not.toHaveProperty('requestMessages')
    expect(zhCN).not.toHaveProperty('requestMessages')
  })

  it('the migrated message keys stay in the agentSessions domain of both locale trees', () => {
    // The bubble renderer reads its disclosure-row copy (expand/collapse +
    // the character-count note) from agentSessions now, and the stream-
    // merged note rides the same domain for the replay surface — removing
    // the page must not take the migrated copy with it.
    for (const tree of [en, zhCN]) {
      expect(tree.agentSessions).toHaveProperty('collapse')
      expect(tree.agentSessions).toHaveProperty('expandFull')
      expect(tree.agentSessions).toHaveProperty('partCharsNote')
      expect(tree.agentSessions).toHaveProperty('streamMergedNote')
    }
  })
})
