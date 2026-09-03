import type { SelectOption } from 'naive-ui'

// The text/image/video option set behind every output-modality picker. The
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
  ]
}

// enforceVideoExclusivity is the watch body every output-modality picker
// runs: once video is in the selection the declaration collapses to video
// alone — video is sticky, and leaving it means deselecting video itself.
// Returning the corrected list keeps one rule in one place instead of
// re-deriving it in every dialog that ever offers the choice.
export function enforceVideoExclusivity(modalities: string[]): string[] {
  if (modalities.includes('video')) {
    return modalities.length > 1 ? ['video'] : modalities
  }
  return modalities
}
