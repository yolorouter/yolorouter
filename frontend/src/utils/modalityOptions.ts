import type { SelectOption } from 'naive-ui'

// The text/image option pair behind every output-modality picker. The values
// ARE the modality ids the server stores — the plain-string bridge the model
// row documents — so nothing here translates between two vocabularies. One
// builder keeps the pickers from drifting apart; the import dialog composes
// its 'both' shorthand on top of the pair.
export function outputModalityOptions(t: (key: string) => string): SelectOption[] {
  return [
    { label: t('models.outputModalityText'), value: 'text' },
    { label: t('models.outputModalityImage'), value: 'image' },
  ]
}
