// frontend/src/utils/modalityBadge.ts
//
// The model-list/detail modality badge rule: text-only models carry no badge
// (text is the default modality — badging every row says nothing), an
// image-only model badges as its image label, a both model as its
// text+image label (the i18n keys below render those words per locale).
// One home so the list page, the detail header, and any future surface
// agree on the rule; the import/create/edit modals keep their own
// three-option picker, which uses the same vocabulary.

type Translator = (key: string) => string

// The badge a model's output_modalities renders as: null = no badge (text
// only). Input is the server's canonical list; order-insensitive.
export function modalityBadge(modalities: string[]): { key: 'image' | 'both' } | null {
  const hasText = modalities.includes('text')
  const hasImage = modalities.includes('image')
  if (hasText && hasImage) return { key: 'both' }
  if (hasImage) return { key: 'image' }
  return null
}

export function modalityBadgeLabel(badge: { key: 'image' | 'both' }, t: Translator): string {
  return t(badge.key === 'image' ? 'models.modalityBadgeImage' : 'models.modalityBadgeBoth')
}
