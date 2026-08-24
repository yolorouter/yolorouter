import { describe, expect, it } from 'vitest'

import { passwordStrengthRule } from './authValidators'
import { generatePassword } from './passwordGenerator'

// The generator's contract is "whatever comes out passes the rule the form
// validates against", so assert exactly that instead of re-deriving the
// character classes here. Wiring the two together is the point: if either
// side drifts, this goes red rather than leaving a generator that quietly
// emits values its own form rejects.
const rule = passwordStrengthRule((key) => key)
const accepted = (value: string) =>
  (rule.validator as (r: unknown, v: string) => boolean)(rule, value)

// Enough draws that a per-draw failure probability worth caring about would
// show up, while staying instant.
const DRAWS = 2000

describe('generatePassword', () => {
  it('produces a password the shared strength rule accepts, every time', () => {
    for (let i = 0; i < DRAWS; i++) {
      const password = generatePassword()
      expect(accepted(password), `rule rejected ${password}`).toBe(true)
    }
  })

  it('carries both required character classes by construction', () => {
    for (let i = 0; i < DRAWS; i++) {
      const password = generatePassword()
      expect(/\p{L}/u.test(password), `no letter in ${password}`).toBe(true)
      expect(/\p{Nd}/u.test(password), `no digit in ${password}`).toBe(true)
    }
  })

  it('keeps its letter even when every pool draw yields a digit', () => {
    // The real source is free to hand back an all-digit pool sample; it just
    // does so about once in 10^12 draws, which no amount of sampling above
    // would catch. Forcing the largest index every time reproduces that draw
    // deterministically: the pool's last entry is a digit, so a generator
    // that seeded only a digit and trusted the pool for the letter emits an
    // all-digit password here — and the form rule rejects it.
    const alwaysLastIndex = (size: number) => size - 1
    const password = generatePassword(alwaysLastIndex)
    expect(/\p{L}/u.test(password), `no letter in ${password}`).toBe(true)
    expect(accepted(password), `rule rejected ${password}`).toBe(true)
  })

  it('does not pin the seeded pair to fixed positions', () => {
    // Without the shuffle the first character would always be a letter and
    // the second always a digit, which is a pattern an attacker gets for
    // free. Across this many draws both classes must appear in slot 0.
    const firstClasses = new Set<string>()
    for (let i = 0; i < 300; i++) {
      firstClasses.add(/\d/.test(generatePassword()[0]) ? 'digit' : 'letter')
    }
    expect(firstClasses).toEqual(new Set(['digit', 'letter']))
  })
})
