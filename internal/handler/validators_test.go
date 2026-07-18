package handler

import (
	"testing"

	"github.com/gin-gonic/gin/binding"
)

// fakeNonValidatorEngine is a StructValidator whose Engine() does NOT return
// a *validator.Validate — exercises RegisterValidators' "fails open rather
// than panicking" branch, which real gin/validator-v10 setups never take
// (see RegisterValidators' own doc comment) and so can only be reached by
// swapping the package-level binding.Validator itself.
type fakeNonValidatorEngine struct{}

func (fakeNonValidatorEngine) ValidateStruct(any) error { return nil }
func (fakeNonValidatorEngine) Engine() any              { return "not a *validator.Validate" }

func TestRegisterValidatorsFailsOpenForNonValidatorEngine(t *testing.T) {
	original := binding.Validator
	binding.Validator = fakeNonValidatorEngine{}
	defer func() { binding.Validator = original }()

	if err := RegisterValidators(); err != nil {
		t.Fatalf("expected RegisterValidators to fail open (nil error) for a non-validator/v10 engine, got %v", err)
	}
}

// TestCleanBindValidationError exercises cleanBindValidationError directly
// (bypassing the HTTP layer, which can only ever produce the "well-formed
// validator.ValidationErrors" and "tag present" shapes — see individual case
// comments for why the others need a hand-crafted string instead).
func TestCleanBindValidationError(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{
			name: "NotAValidationError_ReturnedUnchanged",
			// No "Error:Field validation" substring at all — this is the
			// one shape bindJSON's real call site could theoretically
			// produce (an unrecognized ShouldBindJSON failure mode none of
			// its four earlier type-switches caught) but none of today's
			// binding tags/inputs actually trigger.
			msg:  "some totally unrelated error",
			want: "some totally unrelated error",
		},
		{
			name: "NoQuoteAfterErrorColon_FallsBackToInvalidParameter",
			// Contains the marker substring but no "'" anywhere after it,
			// so the field-name extraction can never start. Real
			// validator-v10 output always quotes the field name, so this
			// shape is synthetic-only, exercising the defensive fallback.
			msg:  "Error:Field validation failed with no quotes at all",
			want: "invalid parameter",
		},
		{
			name: "UnterminatedFieldQuote_FallsBackToInvalidParameter",
			// One opening quote for the field name but no closing quote —
			// again synthetic-only; real validator-v10 output always
			// closes the quote it opens.
			msg:  "Key: 'X' Error:Field validation for 'UnterminatedField",
			want: "invalid parameter",
		},
		{
			name: "NoFailedOnTheTagMarker_FallsBackToFieldInvalid",
			// A properly quoted field name but no "failed on the '...'"
			// tag marker — the one branch bindJSON's real four-way
			// dispatch cannot reach today (every actual validator-v10
			// error always includes a tag), covering the final
			// `field + ": invalid"` fallback line.
			msg:  "Error:Field validation for 'CustomField' due to an unusual reason",
			want: "CustomField: invalid",
		},
		{
			name: "WellFormedValidatorError_ExtractsFieldAndTag",
			// The real shape validator-v10's ValidationErrors.Error()
			// actually produces — mirrors
			// TestSetupRejectsPasswordWithoutDigit's HTTP-level assertion.
			msg:  "Key: 'setupRequest.Password' Error:Field validation for 'Password' failed on the 'bcrypt_len' tag",
			want: "Password: bcrypt_len",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanBindValidationError(tc.msg)
			if got != tc.want {
				t.Fatalf("cleanBindValidationError(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}
