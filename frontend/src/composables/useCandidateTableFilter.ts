// frontend/src/composables/useCandidateTableFilter.ts
import { computed, ref, watch, type Ref } from 'vue'
import { VERIFICATION_FAILED, VERIFICATION_PASSED, VERIFICATION_UNTESTED } from '../api/candidateStatus'

/** The verification-result filter's three buckets, in the menu's order. */
export type CandidateVerificationBucket = 'passed' | 'failed' | 'unverified'

// Bucket -> raw verification_status. The stored three-value field IS the
// bucket key: an "inconclusive" row (probed, no verdict — rate limited,
// unreachable, …) and a never-probed row both store UNTESTED, so both land
// in the unverified bucket with no extra derivation. The five-state progress
// view (candidateProgressState) is deliberately NOT consulted here — it
// folds in queue and promise state a result filter must not depend on.
const BUCKET_STATUS: Record<CandidateVerificationBucket, number> = {
  passed: VERIFICATION_PASSED,
  failed: VERIFICATION_FAILED,
  unverified: VERIFICATION_UNTESTED,
}

/** The bucket options for a FilterSelectField, in passed/failed/unverified order. */
export function candidateVerificationFilterOptions(t: (key: string) => string) {
  return [
    { label: t('providers.verificationPassed'), value: 'passed' },
    { label: t('providers.verificationFailed'), value: 'failed' },
    { label: t('providers.verificationUnverified'), value: 'unverified' },
  ] satisfies Array<{ label: string; value: CandidateVerificationBucket }>
}

/**
 * Search + verification-result filter shared by the two mapping tables —
 * the provider detail page's models tab and the model detail page's route
 * tab — so their behavior stays identical by construction rather than by
 * mirroring. Both tables are fully fetched and paged client-side, so this
 * filters the WHOLE list inside a computed, never the current page's slice.
 *
 * The keyword is a case-insensitive substring over the caller-supplied name
 * fields (each table passes the two name columns it shows); the bucket is a
 * raw verification_status equality. The two conditions intersect, and the
 * page is wired in so any filter change restarts at page 1 — a narrowed list
 * can leave the current page past the last result, and page 2 of everything
 * is never page 2 of the hits.
 */
export function useCandidateTableFilter<Row extends { verification_status: number }>(
  rows: Ref<Row[]>,
  nameFields: (row: Row) => string[],
  pagination: { page: number },
) {
  const keyword = ref('')
  const bucket = ref<CandidateVerificationBucket | null>(null)
  const filteredRows = computed(() => {
    const q = keyword.value.trim().toLowerCase()
    const status = bucket.value === null ? null : BUCKET_STATUS[bucket.value]
    return rows.value.filter((row) => {
      if (status !== null && row.verification_status !== status) return false
      if (q !== '' && !nameFields(row).some((name) => name.toLowerCase().includes(q))) return false
      return true
    })
  })
  watch([keyword, bucket], () => {
    pagination.page = 1
  })
  return { keyword, bucket, filteredRows }
}
