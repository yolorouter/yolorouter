// Builds the system prompt the console installs when Concise Output is
// switched on. Two call sites need it — the global switch and the per-key
// override — and both used to concatenate the two built-in examples inline.

// Sentence joiner for the console's language. Chinese runs sentences
// together after the full-width stop, so an inserted space would be wrong;
// English needs one, and without it the two examples collide into
// "...technical detail.Prefer standard library...".
function sentenceJoiner(locale: string): string {
  return locale.startsWith('zh') ? '' : ' '
}

// t is passed in rather than imported so this stays a pure function the
// tests can drive without an i18n instance.
export function defaultConcisePrompt(t: (key: string) => string, locale: string): string {
  return (
    t('costOptimization.exampleConciseText') +
    sentenceJoiner(locale) +
    t('costOptimization.exampleMinimalCodeText')
  )
}
