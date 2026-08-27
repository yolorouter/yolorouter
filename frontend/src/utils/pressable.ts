// Keyboard/pointer affordance helpers for elements that act as controls but
// are not native buttons. Lives on its own so consumers (deep-link cards,
// hint tags, popover triggers) don't have to import an unrelated module to
// get the accessibility contract.

// activateOnKey is the keydown half of pressable: the key set a button
// answers to, and the guard that keeps a focusable child's own Enter/Space
// from activating its container. Separate from pressable so an element whose
// click is already handled for it — a popover trigger, which the popover
// wires itself — reuses the same keyboard contract instead of re-deriving it
// and dropping the guard. The activated element is passed through for
// handlers that need it.
export function activateOnKey(activate: (el: HTMLElement) => void) {
  return (e: KeyboardEvent) => {
    // Only the element itself: a focusable child (a help icon) bubbles its
    // own Enter/Space up here, and click.stop does not stop keydown.
    if (e.target !== e.currentTarget) return
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      activate(e.currentTarget as HTMLElement)
    }
  }
}

// pressable is the attribute/listener bundle that makes a non-interactive
// element (a KPI card, a ranking row) behave as a real control: pointer
// affordance, button semantics, and keyboard activation. Spread with v-bind.
export function pressable(activate: () => void): Record<string, unknown> {
  return {
    role: 'button',
    tabindex: 0,
    style: 'cursor: pointer',
    onClick: activate,
    onKeydown: activateOnKey(() => activate()),
  }
}
