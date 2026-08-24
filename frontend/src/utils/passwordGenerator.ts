// frontend/src/utils/passwordGenerator.ts
//
// Initial-password generator for the admin "add user" form. Whatever this
// produces has to satisfy passwordStrengthRule in ./authValidators — and
// through it the backend validators that rule mirrors — on every single
// draw, not merely on most of them. Both required character classes are
// therefore placed by construction rather than left to chance.

const DIGITS = '0123456789'
// l, I and O are left out: an initial password is read off one screen and
// typed by hand on another, and those three are the classic misreads.
const LETTERS = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ'
const POOL = LETTERS + DIGITS
const LENGTH = 16

// randomIndex draws a uniform index in [0, size) from crypto random bytes
// via rejection sampling: 256 is not a multiple of size, so a plain modulo
// would favour the first (256 % size) entries. Bytes at or above the
// largest exact multiple are redrawn instead of mapped.
function randomIndex(size: number): number {
  const limit = Math.floor(256 / size) * size
  const b = new Uint8Array(1)
  do {
    crypto.getRandomValues(b)
  } while (b[0] >= limit)
  return b[0] % size
}

/**
 * generatePassword returns a random password that always carries at least
 * one letter and at least one digit. One of each is seeded first and the
 * remainder drawn from the combined pool; a Fisher-Yates shuffle then moves
 * the seeded pair off its fixed positions, so the guarantee costs no
 * predictability.
 *
 * draw is injectable because the guarantee is only worth stating if it can
 * be checked against a hostile source. Sampling the real one proves little:
 * a build that trusted the pool to supply the letter would still pass
 * millions of draws before emitting the all-digit password it permits.
 */
export function generatePassword(draw: (size: number) => number = randomIndex): string {
  const chars = [LETTERS[draw(LETTERS.length)], DIGITS[draw(DIGITS.length)]]
  while (chars.length < LENGTH) {
    chars.push(POOL[draw(POOL.length)])
  }
  for (let i = chars.length - 1; i > 0; i--) {
    const j = draw(i + 1)
    ;[chars[i], chars[j]] = [chars[j], chars[i]]
  }
  return chars.join('')
}
