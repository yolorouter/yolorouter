import type { FormItemRule } from 'naive-ui'

// bcrypt (golang.org/x/crypto/bcrypt) silently truncates/rejects anything
// past 72 bytes — must match internal/handler/validators.go's `bcrypt_len`
// Gin validator exactly, or a password the frontend accepts could still be
// rejected by the backend.
const BCRYPT_MAX_BYTES = 72

/**
 * Password strength rule shared by SetupPage.vue and DefaultLayout.vue's
 * change-password form — must match the backend's `alnum_mixed`/`bcrypt_len`
 * Gin validators (internal/handler/validators.go) EXACTLY, character class
 * and counting semantics included, or a password accepted by one side can
 * be rejected by the other:
 * - Length: Go's `min=10` counts Unicode code points (utf8.RuneCountInString),
 *   not bytes — matched here via Array.from(value).length rather than
 *   value.length, which counts UTF-16 code units and over-counts any
 *   character outside the Basic Multilingual Plane (surrogate pairs).
 * - Letter/digit: Go's unicode.IsLetter/unicode.IsDigit accept any Unicode
 *   letter/decimal-digit, not just ASCII — matched here via the \p{L}
 *   (Letter) and \p{Nd} (Decimal Number) Unicode property escapes, not
 *   [A-Za-z]/\d.
 * - Byte cap: bcrypt's 72-byte limit is a true byte count (TextEncoder),
 *   independent of the two rules above.
 */
export function passwordStrengthRule(t: (key: string) => string): FormItemRule {
  return {
    required: true,
    validator: (_rule, value: string) =>
      Array.from(value).length >= 10 &&
      new TextEncoder().encode(value).length <= BCRYPT_MAX_BYTES &&
      /\p{L}/u.test(value) &&
      /\p{Nd}/u.test(value),
    message: t('auth.passwordRuleMessage'),
    trigger: ['blur', 'input'],
  }
}

/** Confirm-password rule: value must equal whatever getOriginal() returns at validation time. */
export function confirmPasswordRule(t: (key: string) => string, getOriginal: () => string): FormItemRule {
  return {
    required: true,
    validator: (_rule, value: string) => value === getOriginal(),
    message: t('auth.confirmPasswordMismatch'),
    trigger: ['blur', 'input'],
  }
}

/**
 * Username format rule used by SetupPage.vue — must match the backend's
 * `alnum_dash` Gin validator (internal/handler/validators.go's
 * usernamePattern) EXACTLY: 3-32 ASCII letters, digits, hyphens, and
 * underscores. Centralized here (rather than inline in the page) for the
 * same reason as passwordStrengthRule/confirmPasswordRule: a single place
 * to update if the backend rule ever changes.
 */
export function usernameFormatRule(t: (key: string) => string): FormItemRule {
  return {
    required: true,
    validator: (_rule, value: string) => /^[a-zA-Z0-9_-]{3,32}$/.test(value),
    message: t('auth.usernameRuleMessage'),
    trigger: ['blur', 'input'],
  }
}
