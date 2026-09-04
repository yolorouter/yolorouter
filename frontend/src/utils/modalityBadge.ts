// frontend/src/utils/modalityBadge.ts
//
// The model-list/detail modality badge rule: text-only models carry no badge
// (text is the default modality — badging every row says nothing), an
// image-only model badges as its image label, a both model as its
// text+image label, and a video model as its video label (video is an
// exclusive modality server-side, so there is no video+anything combo to
// name). The i18n keys below render those words per locale. One home so
// the list page, the detail header, and any future surface agree on the
// rule; the import/create/edit modals keep their own pickers, which use
// the same vocabulary.

type Translator = (key: string) => string

// The badge a model's output_modalities renders as: null = no badge (text
// only). Input is the server's canonical list; order-insensitive.
export function modalityBadge(modalities: string[]): { key: 'image' | 'both' | 'video' | 'audio' } | null {
  const hasText = modalities.includes('text')
  const hasImage = modalities.includes('image')
  if (modalities.includes('video')) return { key: 'video' }
  if (modalities.includes('audio')) return { key: 'audio' }
  if (hasText && hasImage) return { key: 'both' }
  if (hasImage) return { key: 'image' }
  return null
}

export function modalityBadgeLabel(badge: { key: 'image' | 'both' | 'video' | 'audio' }, t: Translator): string {
  if (badge.key === 'image') return t('models.modalityBadgeImage')
  if (badge.key === 'video') return t('models.modalityBadgeVideo')
  if (badge.key === 'audio') return t('models.modalityBadgeAudio')
  return t('models.modalityBadgeBoth')
}
