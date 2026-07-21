import { createI18n } from 'vue-i18n'
import zhCN from './locales/zh-CN'
import en from './locales/en'

export type Locale = 'zh-CN' | 'en'

const STORAGE_KEY = 'yolorouter-locale'

// Guards against a corrupted/stale localStorage value (or one written by a
// future version of this app with more locales) — an un-normalized value
// would still get accepted as i18n.global.locale.value, silently breaking
// anything that expects to match it exactly against 'zh-CN'/'en' (e.g. a
// locale switcher highlighting the active option would end up with none
// selected).
function normalizeLocale(value: string | null): Locale {
  return value === 'en' ? 'en' : 'zh-CN'
}

function applyDocumentLang(locale: Locale) {
  document.documentElement.lang = locale
}

const initialLocale = normalizeLocale(localStorage.getItem(STORAGE_KEY))
applyDocumentLang(initialLocale)

export const i18n = createI18n({
  legacy: false,
  locale: initialLocale,
  fallbackLocale: 'zh-CN',
  messages: { 'zh-CN': zhCN, en },
})

/** Switches the active locale, persists it, and keeps <html lang> in sync. */
export function setLocale(locale: Locale) {
  i18n.global.locale.value = locale
  localStorage.setItem(STORAGE_KEY, locale)
  applyDocumentLang(locale)
}

export function errcodeMessage(code: number): string {
  const dict = (i18n.global.locale.value === 'en' ? en : zhCN).errcodes
  return dict[code] ?? `error ${code}`
}

/** Looks up a key in the `common` namespace for the active locale (e.g. `t('networkError')`). */
export function t(key: keyof typeof zhCN.common): string {
  const dict = (i18n.global.locale.value === 'en' ? en : zhCN).common
  return dict[key] ?? key
}
