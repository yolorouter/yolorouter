package repository

import (
	"errors"
	"testing"
)

func TestIsUniqueViolationDetectsKnownDatabaseMessages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"sqlite", errors.New("UNIQUE constraint failed: providers.name"), true},
		{"postgres", errors.New(`duplicate key value violates unique constraint "providers_name_key"`), true},
		{"unrelated", errors.New("some other database error"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsUniqueViolation(c.err); got != c.want {
				t.Fatalf("IsUniqueViolation(%q) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestIsSortOrderUniqueViolationNarrowsToTheSortOrderConstraint pins the
// two-layer classification: a sort_order collision is a unique violation
// AND names the sort_order column, while the sibling label/model UNIQUEs on
// the same tables must stay in the broader bucket.
func TestIsSortOrderUniqueViolationNarrowsToTheSortOrderConstraint(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"sqlite sort_order", errors.New("UNIQUE constraint failed: provider_keys.provider_id, provider_keys.sort_order"), true},
		{"postgres sort_order", errors.New(`duplicate key value violates unique constraint "provider_keys_provider_id_sort_order_key"`), true},
		{"label unique is not sort_order", errors.New("UNIQUE constraint failed: provider_keys.provider_id, provider_keys.label"), false},
		{"non-unique error naming sort_order", errors.New("syntax error near sort_order"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSortOrderUniqueViolation(c.err); got != c.want {
				t.Fatalf("IsSortOrderUniqueViolation(%q) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
