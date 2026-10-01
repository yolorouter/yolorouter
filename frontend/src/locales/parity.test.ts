import { describe, expect, it } from 'vitest'
import zhCN from './zh-CN'
import en from './en'
import ruRU from './ru-RU'

// Locale parity gate: every locale must expose the exact same message tree.
// Keys are compared in both directions, and every leaf's named placeholders
// ({name}, {count}, ...) must match across locales — a missing placeholder
// renders as a literal hole in the UI and is otherwise silent.

type MessageTree = Record<string, unknown>

function leafPaths(tree: MessageTree, prefix = ''): Map<string, string> {
  const paths = new Map<string, string>()
  for (const [key, value] of Object.entries(tree)) {
    const path = prefix ? `${prefix}.${key}` : key
    if (value !== null && typeof value === 'object') {
      for (const [nested, message] of leafPaths(value as MessageTree, path)) {
        paths.set(nested, message)
      }
    } else {
      paths.set(path, String(value))
    }
  }
  return paths
}

// Only identifier-shaped params ({name}, {0}) are part of the signature, so
// vue-i18n literal-brace escapes ({'{'}) never match; prose that merely
// looks like a placeholder still counts, which errs on the strict side.
function placeholderSignature(message: string): string {
  const names = [...message.matchAll(/\{([A-Za-z0-9_]+)\}/g)].map((m) => m[1])
  return [...new Set(names)].sort().join(',')
}

const locales: Record<string, MessageTree> = {
  'zh-CN': zhCN as MessageTree,
  en: en as MessageTree,
  'ru-RU': ruRU as MessageTree,
}

describe('locale parity', () => {
  const reference = leafPaths(locales['zh-CN'])
  const referenceName = 'zh-CN'

  for (const [name, tree] of Object.entries(locales)) {
    const paths = leafPaths(tree)

    it(`${name} has no keys missing from ${referenceName}`, () => {
      const missing = [...paths.keys()].filter((path) => !reference.has(path))
      expect(missing, `keys present in ${name} but absent from ${referenceName}`).toEqual([])
    })

    it(`${referenceName} has no keys missing from ${name}`, () => {
      const missing = [...reference.keys()].filter((path) => !paths.has(path))
      expect(missing, `keys present in ${referenceName} but absent from ${name}`).toEqual([])
    })

    it(`every message uses the same named placeholders in ${name} and ${referenceName}`, () => {
      const drifted: string[] = []
      for (const [path, message] of reference) {
        const other = paths.get(path)
        // Missing keys are the two tests above; here only pairs are compared.
        if (other === undefined) continue
        if (placeholderSignature(message) !== placeholderSignature(other)) drifted.push(path)
      }
      expect(drifted, `placeholder sets differ between ${referenceName} and ${name}`).toEqual([])
    })
  }
})