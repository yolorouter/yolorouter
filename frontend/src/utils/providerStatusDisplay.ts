import type { ProviderKey } from '../api/providers'

// Shared by ProviderListPage.vue and CandidateEditModal.vue so the provider
// running_status → i18n key (and NTag color) mapping is defined once — the
// provider-side counterpart of modelStatusDisplay.ts.
export const PROVIDER_RUNNING_STATUS_DISPLAY: Record<
  string,
  { i18nKey: string; type: 'default' | 'success' | 'warning' | 'error' }
> = {
  not_configured: { i18nKey: 'NotConfigured', type: 'default' },
  pending_test: { i18nKey: 'Pending', type: 'default' },
  available: { i18nKey: 'Available', type: 'success' },
  partial: { i18nKey: 'Partial', type: 'warning' },
  unavailable: { i18nKey: 'Unavailable', type: 'error' },
}

export function providerRunningStatusDisplay(status: string) {
  return PROVIDER_RUNNING_STATUS_DISPLAY[status] ?? PROVIDER_RUNNING_STATUS_DISPLAY.unavailable
}

// isKeyUsable mirrors the backend's routable-key rule (model_service.go's
// providerHasAvailableKey): enabled + verification passed + authorized for the
// provider's current destination version — needs_reentry is the wire-level
// negation of that version match, so the three fields below reproduce the rule
// exactly.
export function isKeyUsable(k: ProviderKey): boolean {
  return k.management_status === 1 && k.verification_status === 1 && !k.needs_reentry
}

export function usableKeyCount(keys: ProviderKey[]): number {
  return keys.filter(isKeyUsable).length
}

// The key is usable and no other usable key remains — the shared escalation
// trigger of both key danger flows (disabling it, deleting it), kept next to
// isKeyUsable so the "usable" rule has exactly one home.
export function isLastUsableKey(keys: ProviderKey[], keyId: number): boolean {
  const target = keys.find((k) => k.id === keyId)
  return target !== undefined && isKeyUsable(target) && usableKeyCount(keys) === 1
}

// Whether deleting the given key is what MAKES the provider unable to
// serve: it is the provider's last key at all, or the last usable one while
// every other key is merely present but not routable. Deleting an unusable
// key from an already-unusable pool reports false on purpose — the provider
// serves nothing before and after, so there is no escalation to announce.
// An id not in the list reports false too: the dialog then shows the plain
// copy, and the server answers not-found.
export function deleteLeavesProviderUnusable(keys: ProviderKey[], keyId: number): boolean {
  const target = keys.find((k) => k.id === keyId)
  if (!target) return false
  return keys.length === 1 || isLastUsableKey(keys, keyId)
}

// Gate for the provider-deletion danger button: the admin must retype the
// provider's exact name. Strict equality on purpose — no trimming, no case
// folding — so the gate cannot be satisfied by a near-miss of a similarly
// named provider.
export function deleteConfirmUnlocked(input: string, providerName: string): boolean {
  return providerName !== '' && input === providerName
}
