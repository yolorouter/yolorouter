import { watch } from 'vue'
import { enforceVideoExclusivity } from '../utils/modalityOptions'

// Video is an exclusive modality server-side, so every output-modality
// picker collapses its declaration the moment video joins it — the create
// and edit dialogs run the same watch rather than each restating it. The
// form is the modal's own reactive object; the watch writes the collapsed
// list back onto it, exactly as the inline bodies did.
export function useVideoModalityExclusivity(form: { outputModalities: string[] }): void {
  watch(
    () => form.outputModalities,
    (v) => {
      const enforced = enforceVideoExclusivity(v)
      if (enforced !== v) form.outputModalities = enforced
    },
  )
}
