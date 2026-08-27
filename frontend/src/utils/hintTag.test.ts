import { describe, expect, it } from 'vitest'
import { disclosureTag, hintTag } from './hintTag'

// These tests pin the vnode contract a popover trigger depends on. NPopover
// special-cases a popover-family trigger (NTooltip carries a marker flag for
// this) by handing it the click handlers to forward inward; if a hinted
// disclosure tag ever stops surfacing that flag at its top level — e.g. by
// gaining a wrapper component — clicking the tag silently stops opening the
// panel while every type check stays green.
type MarkedVNode = {
  type: { __popover__?: boolean; name?: string }
  props: Record<string, unknown> | null
  children: { trigger?: () => MarkedVNode } | null
}

describe('disclosureTag', () => {
  it('surfaces the popover-family marker at the top level when a hint is present', () => {
    const vnode = disclosureTag({ text: 'x', type: 'error', hint: 'why it failed', expanded: false }) as unknown as MarkedVNode
    expect(vnode.type.__popover__).toBe(true)
  })

  it('renders the bare button-semantics tag when there is no hint', () => {
    const vnode = disclosureTag({ text: 'x', type: 'error', hint: '', expanded: false }) as unknown as MarkedVNode
    expect(vnode.type.__popover__).toBeUndefined()
    expect(vnode.props?.role).toBe('button')
    expect(vnode.props?.tabindex).toBe(0)
  })

  it('reports the open state as an aria-expanded string', () => {
    const openVNode = disclosureTag({ text: 'x', type: 'error', hint: '', expanded: true }) as unknown as MarkedVNode
    const closedVNode = disclosureTag({ text: 'x', type: 'error', hint: '', expanded: false }) as unknown as MarkedVNode
    expect(openVNode.props?.['aria-expanded']).toBe('true')
    expect(closedVNode.props?.['aria-expanded']).toBe('false')
  })

  it('names itself from text and hint for assistive tech', () => {
    const wrapper = disclosureTag({ text: 'Upstream error', type: 'error', hint: 'expand', expanded: false }) as unknown as MarkedVNode
    // The accessible name lives on the tag inside the tooltip wrapper (the
    // wrapper's own props carry none), reachable through its trigger slot.
    expect(wrapper.props?.['aria-label']).toBeUndefined()
    const tag = wrapper.children?.trigger?.()
    expect(tag?.props?.['aria-label']).toBe('Upstream error: expand')
  })
})

describe('hintTag', () => {
  it('keeps tooltip wrapping consistent with disclosureTag', () => {
    const hinted = hintTag({ text: 'x', type: 'warning', hint: 'why' }) as unknown as MarkedVNode
    const bare = hintTag({ text: 'x', type: 'warning', hint: '' }) as unknown as MarkedVNode
    expect(hinted.type.__popover__).toBe(true)
    expect(bare.type.__popover__).toBeUndefined()
    expect(bare.props?.['aria-label']).toBe('x')
  })
})
