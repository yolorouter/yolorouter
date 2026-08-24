import type { FormItemRule } from 'naive-ui'

// Mirrors the backend's own binding tags (createModelRequest / createCandidateRequest
// in internal/handler/model_handler.go), same convention as
// utils/providerValidators.ts.

// The charset/length check on its own (no required), so callers that make the
// field conditionally optional can compose it with their own required rule
// without re-hardcoding the pattern. async-validator skips a non-required
// pattern rule on empty input.
export function modelNameFormatRule(t: (key: string) => string): FormItemRule {
  return { max: 100, pattern: /^[a-zA-Z0-9._-]+(?:\/[a-zA-Z0-9._-]+)*$/, message: t('models.nameInvalid'), trigger: ['blur', 'input'] }
}

export function modelNameRule(t: (key: string) => string): FormItemRule[] {
  return [
    { required: true, message: t('models.fieldRequired'), trigger: ['blur', 'input'] },
    modelNameFormatRule(t),
  ]
}

// Optional: leaving this blank means "use the model's own external name
// upstream unchanged" — the backend (TestAndCreateCandidate /
// CreateModelCandidate / UpdateModelCandidate in
// internal/service/model_service.go) defaults it to the model's name when empty.
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
