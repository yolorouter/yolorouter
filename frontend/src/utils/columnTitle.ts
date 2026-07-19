// frontend/src/utils/columnTitle.ts

/** Column width that fits a HelpLabel "?" glyph + a one-word tag/switch. */
export const STATUS_COL_WIDTH = 162

import { h } from 'vue'
import type { VNodeChild } from 'vue'
import HelpLabel from '../components/HelpLabel.vue'

/**
 * DataTable column title: the label text + an inline "?" tooltip (reuses
 * HelpLabel so the glyph has one implementation source).
 *
 * Contract: takes already-translated `text` and `tip` (deliberately decoupled
 * from vue-i18n so this stays a pure render utility). Callers pass
 * `t('x')` + `t('x_tip')` and keep the pair in sync. Trade-off vs the
 * reference project's `colTitle(key)` (derives `_tip` internally) — CE keeps
 * the helper i18n-agnostic.
 *
 * Actions columns don't need a tooltip — use a plain `title: t('common.actions')`
 * string there, NOT columnTitle.
 *
 * Mirrors reference: yolorouter-frontend/src/admin/pages/BalanceLedgerPage.vue.
 */
export function columnTitle(text: string, tip: string): () => VNodeChild {
  return () => h(HelpLabel, { tip }, { default: () => text })
}
