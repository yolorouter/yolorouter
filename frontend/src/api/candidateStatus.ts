// Server enum values for a mapping's management_status / verification_status.
// Compare against these, not raw literals — a renumbered server enum should
// break one line here, not scatter silent misclassifications. Kept free of
// imports so pure utility modules (and their node-side tests) can use them
// without dragging in the http client chain — for the same reason, ALWAYS
// import these from this module directly, never re-exported through a barrel.
export const CANDIDATE_STATUS_ENABLED = 1
export const CANDIDATE_STATUS_DISABLED = 2
export const VERIFICATION_UNTESTED = 0
export const VERIFICATION_PASSED = 1
export const VERIFICATION_FAILED = 2
