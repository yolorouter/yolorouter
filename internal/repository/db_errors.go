package repository

import "strings"

// IsUniqueViolation reports whether err is a UNIQUE-constraint violation,
// across both supported drivers: SQLite says "UNIQUE constraint failed",
// Postgres says "duplicate key value violates unique constraint". Callers
// use it to turn a raced insert (two writers passing the same up-front
// existence check) into the domain's name-taken error instead of a raw
// database error.
func IsUniqueViolation(err error) bool {
	msg := err.Error()
	// "23505" is PostgreSQL's SQLSTATE for unique_violation. The driver appends
	// it to the message untranslated, so it still matches when lc_messages
	// localizes the human-readable text the other two patterns rely on.
	return strings.Contains(msg, "UNIQUE constraint") || strings.Contains(msg, "duplicate key value violates unique constraint") ||
		strings.Contains(msg, "23505")
}

// IsTxSerializationFailure reports whether err is a PostgreSQL transaction
// abort that is safe to retry wholesale: a deadlock (40P01) or a
// serialization failure (40001). Both mean the transaction was rolled back
// cleanly because it lost a race with a concurrent transaction — nothing was
// persisted, so a bounded re-run resolves the conflict. SQLite cannot produce
// either (single writer), so this matching is Postgres-only by construction.
func IsTxSerializationFailure(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "40P01") || strings.Contains(msg, "40001") ||
		strings.Contains(msg, "deadlock detected") || strings.Contains(msg, "could not serialize access")
}

// IsSortOrderUniqueViolation narrows IsUniqueViolation to specifically the
// sort_order constraint, as opposed to the other UNIQUE on the same table —
// UNIQUE(provider_id, label) on provider_keys, UNIQUE(model_id, provider_id) on
// model_candidates, which both layers need to report differently. Both SQLite and Postgres
// name unnamed multi-column UNIQUE constraint violations after their
// columns — SQLite: "UNIQUE constraint failed: provider_keys.provider_id,
// provider_keys.sort_order"; Postgres: constraint
// "provider_keys_provider_id_sort_order_key" — so a plain substring check
// on "sort_order" reliably identifies this one across both drivers.
func IsSortOrderUniqueViolation(err error) bool {
	return IsUniqueViolation(err) && strings.Contains(err.Error(), "sort_order")
}
