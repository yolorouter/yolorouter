import { watch } from 'vue'
import { enforceExclusiveModalities } from '../utils/modalityOptions'

// Video and audio are exclusive modalities server-side, so every
// output-modality picker collapses its declaration the moment one of them
// joins it — the create and edit dialogs run the same watch rather than
// each restating it. The form is the modal's own reactive object; the
// watch writes the collapsed list back onto it, exactly as the inline
// bodies did.
export function useModalityExclusivity(form: { outputModalities: string[] }): void {
  watch(
    () => form.outputModalities,
    (v) => {
      const enforced = enforceExclusiveModalities(v)
      if (enforced !== v) form.outputModalities = enforced
    },
  )
}
