import { createI18n } from 'vue-i18n'
import zhCN from './locales/zh-CN'
import en from './locales/en'
import ruRU from './locales/ru-RU'

export type Locale = 'zh-CN' | 'en' | 'ru-RU'

// The user-selectable languages, in display order. Single source of truth so
// the header language switcher and the settings language picker can't drift —
// adding a language is a one-line edit here. Labels are the language's own
// endonym (shown to users), hence the non-English literals.
export const LOCALES: { label: string; value: Locale }[] = [
  { label: 'Русский', value: 'ru-RU' },
  { label: '简体中文', value: 'zh-CN' },
  { label: 'English', value: 'en' },
]

const STORAGE_KEY = 'yolorouter-locale'

// Guards against a corrupted/stale localStorage value (or one written by a
// future version of this app with more locales) — a non-normalized value
// would still be accepted as i18n.global.locale.value, silently breaking
// anything that expects to match it exactly against 'zh-CN'/'en'/'ru-RU' (e.g. a
// locale switcher highlighting the active option would end up with none
// selected).
function normalizeLocale(value: string | null): Locale {
  if (value === 'en' || value === 'ru-RU') return value
  return 'zh-CN'
}

function applyDocumentLang(locale: Locale) {
  document.documentElement.lang = locale
}

// Reflects the active locale as a class on the #app root (e.g. `zh-CN` / `en`)
// so styles can key off the language. Strips every known locale class first so
// switching languages never leaves a stale one behind.
function applyAppLocaleClass(locale: Locale) {
  const app = document.getElementById('app')
  if (!app) return
  app.classList.remove(...LOCALES.map((l) => l.value))
  app.classList.add(locale)
}

/** Returns the currently active locale — the single accessor for reading it. */
export function getLocale(): Locale {
  return i18n.global.locale.value as Locale
}

const initialLocale = normalizeLocale(localStorage.getItem(STORAGE_KEY))
applyDocumentLang(initialLocale)
applyAppLocaleClass(initialLocale)

export const i18n = createI18n({
  legacy: false,
  locale: initialLocale,
  fallbackLocale: 'zh-CN',
  messages: { 'zh-CN': zhCN, en, 'ru-RU': ruRU },
})

/** Switches the active locale, persists it, and keeps <html lang> in sync. */
export function setLocale(locale: Locale) {
  i18n.global.locale.value = locale
  localStorage.setItem(STORAGE_KEY, locale)
  applyDocumentLang(locale)
  applyAppLocaleClass(locale)
}

/** Dictionary for the active locale — the single place that maps locale → messages. */
function activeMessages(): typeof zhCN {
  const locale = getLocale()
  if (locale === 'en') return en
  if (locale === 'ru-RU') return ruRU
  return zhCN
}

export function errcodeMessage(code: number): string {
  const dict = activeMessages().errcodes
  return dict[code] ?? `error ${code}`
}

/** Looks up a key in the `common` namespace for the active locale (e.g. `t('networkError')`). */
export function t(key: keyof typeof zhCN.common): string {
  const dict = activeMessages().common
  return dict[key] ?? key
}