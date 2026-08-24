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

func TestIsTxSerializationFailure(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"ERROR: deadlock detected (SQLSTATE 40P01)", true},
		{"ERROR: could not serialize access due to concurrent update (SQLSTATE 40001)", true},
		{"UNIQUE constraint failed: models.name", false},
		{"connection refused", false},
	}
	for _, tc := range cases {
		if got := IsTxSerializationFailure(errors.New(tc.msg)); got != tc.want {
			t.Errorf("IsTxSerializationFailure(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// PostgreSQL localizes constraint messages under non-English lc_messages, but
// the SQLSTATE code survives translation — detection must key on it too.
func TestIsUniqueViolationDetectsLocalizedPostgresMessageBySQLState(t *testing.T) {
	err := errors.New(`ERROR: 重复键违反唯一约束"models_name_key" (SQLSTATE 23505)`)
	if !IsUniqueViolation(err) {
		t.Fatal("expected a localized 23505 message to be detected as a unique violation")
	}
	if IsUniqueViolation(errors.New("ERROR: some other failure (SQLSTATE 40P01)")) {
		t.Fatal("a non-23505 SQLSTATE must not be treated as a unique violation")
	}
}
