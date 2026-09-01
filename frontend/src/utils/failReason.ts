// frontend/src/utils/failReason.ts
//
// Maps gateway fail_reason codes to user-friendly descriptions. Gateway stores
// structured codes (empty_model, upstream_client_error_400, etc.); these are
// not suitable for direct display. Unknown codes or free-text upstream errors
// (containing ": " or spaces) are returned as-is since they are often already
// semi-readable.

const FAIL_REASON_PREFIXES: { prefix: string; key: string }[] = [
  { prefix: 'upstream_client_error_', key: 'failReason.upstreamClientError' },
  { prefix: 'validate: ', key: 'failReason.validate' },
  { prefix: 'parse: ', key: 'failReason.parse' },
  { prefix: 'invalid_request: ', key: 'failReason.invalidRequest' },
  { prefix: 'db_model: ', key: 'failReason.dbError' },
  { prefix: 'db_allowlist: ', key: 'failReason.dbError' },
  { prefix: 'db_candidates: ', key: 'failReason.dbError' },
  { prefix: 'stream_partial: ', key: 'failReason.streamPartial' },
  { prefix: 'ir_decode_partial: ', key: 'failReason.irDecodePartial' },
]

const FAIL_REASON_CODES: Record<string, string> = {
  empty_model: 'failReason.emptyModel',
  model_not_found: 'failReason.modelNotFound',
  model_disabled: 'failReason.modelDisabled',
  model_not_allowed: 'failReason.modelNotAllowed',
  invalid_gemini_path: 'failReason.invalidGeminiPath',
  body_too_large: 'failReason.bodyTooLarge',
  concurrency_limit: 'failReason.concurrencyLimit',
  rpm_exceeded: 'failReason.rpmExceeded',
  tpm_exceeded: 'failReason.tpmExceeded',
  budget_exceeded: 'failReason.budgetExceeded',
  revoked: 'failReason.revoked',
  expired: 'failReason.expired',
  client_disconnected: 'failReason.clientDisconnected',
  panic_recovered: 'failReason.panicRecovered',
  all_candidates_failed: 'failReason.allCandidatesFailed',
  request_budget_exhausted: 'failReason.requestBudgetExhausted',
  upstream_rate_limited: 'failReason.upstreamRateLimited',
  content_inspection_refused: 'failReason.contentInspectionRefused',
  partial_then_exhausted: 'failReason.partialThenExhausted',
  no_capability_candidate: 'failReason.noCapabilityCandidate',
  no_verified_candidate: 'failReason.noVerifiedCandidate',
  no_enabled_candidate: 'failReason.noEnabledCandidate',
  stream_no_done: 'failReason.streamNoDone',
  empty_prompt: 'failReason.emptyPrompt',
  image_streaming_unsupported: 'failReason.imageStreamingUnsupported',
  model_modality_mismatch: 'failReason.modelModalityMismatch',
  image_response_empty: 'failReason.imageResponseEmpty',
  dashscope_business_error: 'failReason.dashscopeBusinessError',
}

// formatFailReason converts a raw gateway fail_reason into a user-friendly
// localized string. Uses the i18n translator t for known codes; returns
// the original string for unknown codes (often semi-readable upstream errors).
export function formatFailReason(
  reason: string | null | undefined,
  t: (key: string, args?: Record<string, unknown>) => string,
): string {
  if (!reason) return t('failReason.unknown')

  // Exact code match
  const codeKey = FAIL_REASON_CODES[reason]
  if (codeKey) return t(codeKey)

  // Prefix match (e.g. upstream_client_error_400, validate: ...)
  for (const { prefix, key } of FAIL_REASON_PREFIXES) {
    if (reason.startsWith(prefix)) {
      const detail = reason.slice(prefix.length)
      return t(key, { detail })
    }
  }

  // Unknown — return as-is (upstream error messages are often readable)
  return reason
}
