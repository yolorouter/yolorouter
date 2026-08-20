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
	return strings.Contains(msg, "UNIQUE constraint") || strings.Contains(msg, "duplicate key value violates unique constraint")
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
