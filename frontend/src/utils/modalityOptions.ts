import type { SelectOption } from 'naive-ui'

// The text/image/video/audio option set behind every output-modality picker. The
// values ARE the modality ids the server stores — the plain-string bridge
// the model row documents — so nothing here translates between two
// vocabularies. One builder keeps the pickers from drifting apart; the
// import dialog composes its 'both' shorthand on top of text+image and
// filters video out (its rows cannot carry a video price table).
//
// Video is exclusive by the server's own vocabulary (a video model serves
// the videos endpoints and nothing else), so the pickers pair this list
// with enforceVideoExclusivity rather than trusting a multi-select.
export function outputModalityOptions(t: (key: string) => string): SelectOption[] {
  return [
    { label: t('models.outputModalityText'), value: 'text' },
    { label: t('models.outputModalityImage'), value: 'image' },
    { label: t('models.outputModalityVideo'), value: 'video' },
    { label: t('models.outputModalityAudio'), value: 'audio' },
  ]
}

// The exclusive ids: a model declaring one of these serves that endpoint
// family and nothing else, so a selection containing one collapses to it
// alone. Speech joins video for the same server-side reason — an audio
// model answers the speech endpoint, not chat.
const EXCLUSIVE_MODALITIES = ['video', 'audio']

// enforceExclusiveModalities is the watch body every output-modality picker
// runs: once an exclusive id is in the selection the declaration collapses
// to that id alone — exclusivity is sticky, and leaving it means
// deselecting the id itself. Returning the corrected list keeps one rule in
// one place instead of re-deriving it in every dialog that ever offers the
// choice.
export function enforceExclusiveModalities(modalities: string[]): string[] {
  for (const id of EXCLUSIVE_MODALITIES) {
    if (modalities.includes(id)) {
      return modalities.length > 1 ? [id] : modalities
    }
  }
  return modalities
}
