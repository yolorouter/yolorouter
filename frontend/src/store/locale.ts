import { defineStore } from 'pinia'
import { i18n, setLocale as applyLocale, type Locale } from '../i18n'

export const useLocaleStore = defineStore('locale', {
  // Reads i18n.global.locale.value (already normalized against a
  // corrupted/stale localStorage value and synced to <html lang> at module
  // load — see i18n.ts) rather than re-reading localStorage independently,
  // so this store can't end up disagreeing with i18n about the current
  // locale.
  state: () => ({ locale: i18n.global.locale.value as Locale }),
  actions: {
    setLocale(locale: Locale) {
      this.locale = locale
      applyLocale(locale)
    },
  },
})
