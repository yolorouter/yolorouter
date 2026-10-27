import { h } from 'vue'
import type { VNodeChild } from 'vue'

// Visual shell shared by the table expand panels (model route chain,
// provider mapping summary): a soft raised strip with an eyebrow naming the
// block, so the panel reads as a deliberate sub-view instead of loose text
// floating in the row gap. Inline styles throughout because scoped CSS
// cannot reach h()-rendered nodes.
//
// display:flex (block-level, NOT inline-flex): the panel must fill the
// expand cell's full width — an inline-flex strip shrinks to its content
// (the row cards' fixed tracks ≈ 850px), which parked the comparison list
// in the left half of a wide screen.
const PANEL_STYLE =
  'display:flex; flex-direction:column; gap:8px; padding:10px 16px 12px;' +
  ' background:var(--color-bg-soft); border:1px solid var(--color-border-subtle);' +
  ' border-radius:var(--radius-md); max-width:100%;'

const EYEBROW_STYLE =
  'font-size:var(--text-xs); color:var(--color-text-muted); letter-spacing:0.05em;'

// Muted body text for a panel with nothing to show.
export const EXPAND_EMPTY_STYLE = 'font-size:var(--text-sm); color:var(--color-text-muted);'

export function expandPanel(
  opts: {
    /** Section label above the content; omit on mobile, where the card row already carries one. */
    eyebrow?: string
    /** Desktop panels indent past the expand-arrow gutter to sit under the first data column. */
    indent?: boolean
  },
  children: VNodeChild[],
): VNodeChild {
  const margin = opts.indent ? ' margin:2px 0 6px 44px;' : ' margin:2px 0;'
  const kids = opts.eyebrow ? [h('div', { style: EYEBROW_STYLE }, opts.eyebrow), ...children] : children
  return h('div', { style: PANEL_STYLE + margin }, kids)
}

// Row props for a list whose rows both navigate on click AND carry an expand
// column: the expand arrow's click bubbles up to the row, and expanding a row
// must not also navigate away from the list it just opened.
export function rowNavigationProps(navigate: () => void): Record<string, unknown> {
  return {
    style: 'cursor: pointer',
    onClick: (e: MouseEvent) => {
      if ((e.target as HTMLElement | null)?.closest('.n-data-table-td--expand')) return
      navigate()
    },
  }
}
