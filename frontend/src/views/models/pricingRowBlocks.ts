// frontend/src/views/models/pricingRowBlocks.ts
//
// The model pricing page's two vnode hosts. The shared renderers
// (candidatePriceLines / renderProbeStateTag) return vnodes, which a template
// cannot place directly; these single-purpose components host one row's vnode
// output. A separate module (not inline in the page) keeps one component
// identity for every page instance, so rows patch instead of remount.

import { defineComponent, h, type PropType } from 'vue'
import { useI18n } from 'vue-i18n'
import { candidatePriceLines, type PricedCandidate } from '../../utils/candidatePriceLines'
import { renderProbeStateTag } from '../../utils/probeStateTag'
import type { ModelCandidate } from '../../api/models'

// One mapping row's price cell: the shared candidate price renderer, so the
// billing mode decides what shows and the tier summaries format exactly like
// the mapping tables' price columns (no second formatting implementation).
export const PriceLinesBlock = defineComponent({
  name: 'PricingPriceLinesBlock',
  props: { candidate: { type: Object as PropType<PricedCandidate>, required: true } },
  setup(props) {
    const { t } = useI18n()
    return () => h('div', { class: 'pricing-row__price-lines' }, candidatePriceLines(props.candidate, t))
  },
})

// The shared probe six-state badge, with this page's pending fallback: the
// models list carries no queue stamp, so a not-yet-probed mapping shows the
// plain untested label.
export const ProbeTagBlock = defineComponent({
  name: 'PricingProbeTagBlock',
  props: { candidate: { type: Object as PropType<ModelCandidate>, required: true } },
  setup(props) {
    const { t } = useI18n()
    return () =>
      renderProbeStateTag(t, props.candidate as unknown as Parameters<typeof renderProbeStateTag>[1], {
        labelKey: 'providers.verificationUntested',
        type: 'default',
      })
  },
})
