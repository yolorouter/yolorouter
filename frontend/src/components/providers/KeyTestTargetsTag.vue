<!-- frontend/src/components/providers/KeyTestTargetsTag.vue
     A result tag that opens the per-protocol breakdown panel on click. It is
     a component rather than a render helper because the disclosure has state:
     the trigger must report aria-expanded as the popover opens and closes,
     and only a component instance can carry that across the owning table's
     re-renders.

     Deliberately a render-function component, not <script setup> + template:
     the popover trigger must receive the tag builder's vnode — which is an
     NTooltip when a hint is present — as the slot's own top-level node.
     NPopover special-cases a popover-family trigger by handing it the click
     handlers to forward to its inner trigger; any wrapper component in
     between (the only way a template can host a vnode-building helper) masks
     that, the forwarded handlers land on the tooltip body, and the panel
     silently stops opening for exactly the tags that carry a hint. -->
<script lang="ts">
import { computed, defineComponent, h, ref, type PropType } from 'vue'
import { NPopover } from 'naive-ui'
import { useI18n } from 'vue-i18n'
import type { KeyTestTarget } from '../../api/providers'
import { disclosureTag } from '../../utils/hintTag'
import { keyTestTargetRows } from '../../utils/keyTestTargets'
import KeyTestTargetList from './KeyTestTargetList.vue'

export default defineComponent({
  name: 'KeyTestTargetsTag',
  props: {
    text: { type: String, required: true },
    type: { type: String as PropType<'success' | 'warning' | 'error'>, required: true },
    /**
     * Category hint, shown as the trigger's hover tooltip and folded into
     * its accessible name. It is deliberately NOT rendered inside the panel:
     * the hints are written for the closed state ("expand for details"),
     * which read as nonsense on top of an already-expanded panel. Empty
     * means the tag has no reason to offer.
     */
    hint: { type: String, required: true },
    targets: { type: [Array, null] as PropType<KeyTestTarget[] | null>, required: true },
  },
  setup(props) {
    const { t } = useI18n()
    // Mirrors the popover's own visibility (it stays uncontrolled) purely so
    // the trigger can speak its state.
    const open = ref(false)
    // Only read from the popover body, which NPopover mounts on demand — a
    // table full of closed tags never pays the per-destination i18n lookups.
    const rows = computed(() => keyTestTargetRows(t, props.targets))
    return () =>
      h(
        NPopover,
        { trigger: 'click', placement: 'bottom-start', 'onUpdate:show': (v: boolean) => (open.value = v) },
        {
          trigger: () =>
            disclosureTag({ text: props.text, type: props.type, hint: props.hint, expanded: open.value }),
          default: () => h(KeyTestTargetList, { rows: rows.value, expanded: true }),
        },
      )
  },
})
</script>
