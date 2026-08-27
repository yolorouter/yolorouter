// The shared compact result tags: hintTag keeps its reason in a hover
// tooltip, disclosureTag opens a panel on click. Neither gesture is available
// to keyboard and screen-reader users on a plain tag — so both tags are
// focusable, carry their reason as an accessible name, and answer the keys a
// button answers to. Centralized because the pattern had started to drift:
// one copy shipped without any of the accessibility attributes.
import { h, type VNode } from 'vue'
import { NTag, NTooltip } from 'naive-ui'
import { ChevronDown } from '@lucide/vue'
import { activateOnKey } from './pressable'

export interface HintTagOptions {
  /** Visible tag text (may be a bare glyph like '✗'). */
  text: string
  type: 'success' | 'warning' | 'error' | 'default'
  /** Tooltip body; empty renders the tag alone, still with an accessible name. */
  hint: string
  /** Accessible name; defaults to "text: hint" (or text alone when no hint). */
  ariaLabel?: string
}

export interface DisclosureTagOptions extends HintTagOptions {
  /**
   * Whether the panel behind the tag is currently open, reported as
   * aria-expanded so assistive tech hears the state change. The caller owns
   * the disclosure, so it supplies the state.
   */
  expanded: boolean
}

export function hintTag(options: HintTagOptions) {
  const { text, type, hint } = options
  const tag = h(
    NTag,
    { size: 'small', bordered: false, type, role: 'img', 'aria-label': tagAriaLabel(options), tabindex: 0 },
    { default: () => text },
  )
  return withHoverHint(tag, hint)
}

// The shared tooltip tail: a tag with a reason shows it on hover, one
// without renders bare. Extracted so the two tag builders cannot drift on
// how a hint becomes visible.
function withHoverHint(tag: VNode, hint: string) {
  if (!hint) return tag
  return h(NTooltip, { trigger: 'hover' }, { trigger: () => tag, default: () => hint })
}

// disclosureTag is the same tag as a click-to-open trigger: a chevron says it
// opens something, and the tag carries button semantics because an NTag is not
// one — without them the panel behind it would be mouse-only, exactly the gap
// hintTag was centralized to close. The click itself belongs to the disclosure
// the caller wraps this in (a popover binds its own handler to the trigger),
// so the keyboard path replays it as a click rather than duplicating what
// opening means. The hint keeps hintTag's hover tooltip too — the panel does
// not render it (its copy addresses the closed state), so without the
// tooltip the hint would be audible to screen readers but visible to no one.
export function disclosureTag(options: DisclosureTagOptions) {
  const { text, type, hint, expanded } = options
  const tag = h(
    NTag,
    {
      size: 'small',
      bordered: false,
      type,
      class: 'disclosure-tag',
      role: 'button',
      'aria-label': tagAriaLabel(options),
      'aria-expanded': String(expanded),
      tabindex: 0,
      onKeydown: activateOnKey((el) => el.click()),
    },
    { default: () => [h('span', text), h(ChevronDown, { size: 12 })] },
  )
  return withHoverHint(tag, hint)
}

// Both tags name themselves the same way, so a reason shown as a tooltip and
// one shown in a panel read alike to a screen reader.
function tagAriaLabel(options: HintTagOptions): string {
  const { text, hint } = options
  return options.ariaLabel ?? (hint ? `${text}: ${hint}` : text)
}
