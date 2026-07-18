import type { FormItemRule } from 'naive-ui'

// Mirrors the backend's own binding tags (createProviderRequest / createKeyRequest
// / updateKeyRequest in internal/handler/provider_handler.go) so NewProviderDrawer.vue
// and KeyEditDrawer.vue can't drift apart from each other or from the backend —
// the same reason authValidators.ts exists for the M1 auth forms (a /simplify
// reuse-review finding: these rules were duplicated inline in both drawers).

export function providerNameRule(t: (key: string) => string): FormItemRule[] {
  return [
    { required: true, message: t('providers.fieldRequired'), trigger: ['blur', 'input'] },
    { min: 2, max: 50, message: t('providers.nameLengthHint'), trigger: ['blur', 'input'] },
  ]
}

export function baseUrlRule(t: (key: string) => string): FormItemRule[] {
  return [
    { required: true, message: t('providers.fieldRequired'), trigger: ['blur', 'input'] },
    { type: 'url', max: 255, message: t('providers.baseUrlInvalid'), trigger: ['blur', 'input'] },
  ]
}

export function noteRule(t: (key: string) => string): FormItemRule[] {
  return [{ max: 200, message: t('providers.noteTooLong'), trigger: ['blur', 'input'] }]
}

export function keyLabelRule(t: (key: string) => string): FormItemRule[] {
  return [
    { required: true, message: t('providers.fieldRequired'), trigger: ['blur', 'input'] },
    { min: 2, max: 30, message: t('providers.labelLengthHint'), trigger: ['blur', 'input'] },
  ]
}

// required is parameterized the same way confirmPasswordRule's getOriginal is:
// a brand-new key's plaintext is mandatory, but an existing key's edit form
// leaves it blank to mean "keep the current key" (design doc §8).
export function keyPlaintextRule(t: (key: string) => string, required: boolean): FormItemRule[] {
  return [
    { required, message: t('providers.fieldRequired'), trigger: ['blur', 'input'] },
    { min: 20, message: t('providers.keyPlaintextTooShort'), trigger: ['blur', 'input'] },
  ]
}

export function testModelRule(t: (key: string) => string): FormItemRule[] {
  return [
    { required: true, message: t('providers.fieldRequired'), trigger: ['blur', 'input'] },
    { max: 100, message: t('providers.testModelTooLong'), trigger: ['blur', 'input'] },
  ]
}
