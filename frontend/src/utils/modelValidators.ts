import type { FormItemRule } from 'naive-ui'

// Mirrors the backend's own binding tags (createModelRequest / createCandidateRequest
// in internal/handler/model_handler.go), same convention as
// utils/providerValidators.ts established in M2.

export function modelNameRule(t: (key: string) => string): FormItemRule[] {
  return [
    { required: true, message: t('models.fieldRequired'), trigger: ['blur', 'input'] },
    { max: 100, pattern: /^[a-zA-Z0-9._-]+$/, message: t('models.nameInvalid'), trigger: ['blur', 'input'] },
  ]
}

// Optional: leaving this blank means "use the model's own external name
// upstream unchanged" — the backend (CreateModelCandidate/UpdateModelCandidate/
// TestCandidateMappingPreview in internal/service/model_service.go) defaults
// it to the model's name when empty.
export function providerModelNameRule(t: (key: string) => string): FormItemRule[] {
  return [
    { max: 200, message: t('models.fieldRequired'), trigger: ['blur', 'input'] },
  ]
}

export function nonNegativePriceRule(t: (key: string) => string): FormItemRule[] {
  return [
    { required: true, type: 'number', min: 0, message: t('models.priceNonNegative'), trigger: ['blur', 'input', 'change'] },
  ]
}
