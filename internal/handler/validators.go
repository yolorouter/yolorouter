// Package handler is the HTTP layer for the auth module: Gin binding +
// validation, calling internal/service for all business logic, and mapping
// service-layer errors to the unified response envelope.
package handler

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
)

var (
	usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)
)

// RegisterValidators adds this package's custom `binding` tags
// ("alnum_dash", "alnum_mixed", "bcrypt_len") to Gin's global validator
// engine. Must be called exactly once before any route using these tags is
// registered — internal/router.New calls it.
func RegisterValidators() error {
	v, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		return nil // non-validator/v10 engine (never true in this project, but fails open rather than panicking)
	}
	if err := v.RegisterValidation("alnum_dash", validateAlnumDash); err != nil {
		return err
	}
	if err := v.RegisterValidation("alnum_mixed", validateAlnumMixed); err != nil {
		return err
	}
	return v.RegisterValidation("bcrypt_len", validateBcryptLen)
}

// validateAlnumDash implements the admin username charset: 3-32 letters,
// digits, hyphens, and underscores. The `{3,32}` in
// usernamePattern already enforces the length bound too, not just the
// character class — the struct tags' `min=3,max=32` duplicates the same
// length limit as defense in depth (so a caller that binds a request
// without going through Gin's validator tags still gets rejected). These
// are NOT independent: if this length rule ever changes, both
// usernamePattern and every `min=3,max=32` struct tag must be updated
// together, or the two will silently disagree.
func validateAlnumDash(fl validator.FieldLevel) bool {
	return usernamePattern.MatchString(fl.Field().String())
}

// validateAlnumMixed implements the admin password rule: must contain at
// least one letter AND at least one digit. Length is enforced
// separately via `min=10`/`bcrypt_len` in the struct tags.
func validateAlnumMixed(fl validator.FieldLevel) bool {
	s := fl.Field().String()
	hasLetter, hasDigit := false, false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	return hasLetter && hasDigit
}

// bcryptMaxBytes is bcrypt's own hard limit: golang.org/x/crypto/bcrypt
// (built on Blowfish) silently truncates any input past 72 bytes, and
// GenerateFromPassword returns ErrPasswordTooLong for inputs longer than
// that. Rejecting here — before ever reaching bcrypt — turns what would
// otherwise surface as a confusing generic DatabaseError into a clear,
// specific validation message.
const bcryptMaxBytes = 72

// validateBcryptLen rejects a password/new_password value bcrypt can't
// hash. Checked in bytes (Go's len(string) is already a byte count), not
// runes — multi-byte UTF-8 characters hit this ceiling well before 72
// characters.
func validateBcryptLen(fl validator.FieldLevel) bool {
	return len(fl.Field().String()) <= bcryptMaxBytes
}

// cleanBindValidationError rewrites Gin/validator's raw bind-failure text
// (e.g. "Key: 'setupRequest.Password' Error:Field validation for
// 'Password' failed on the 'bcrypt_len' tag") into a clean "field: tag"
// message, so internal Go struct/field names never leak to the client.
// This ports pkg/response's unexported cleanValidationMessage rather than
// calling it (via pkg/response.ParamError) because ParamError only handles
// this one failure shape — bindJSON's own dispatch (see its doc comment)
// also needs to clean the JSON-type-mismatch, malformed-body, and
// read-timeout shapes, none of which ParamError's cleaning understands.
// Non-validation messages are returned as-is.
func cleanBindValidationError(msg string) string {
	if !strings.Contains(msg, "Error:Field validation") {
		return msg
	}

	_, validationPart, ok := strings.Cut(msg, "Error:")
	if !ok {
		return "invalid parameter"
	}
	validationPart = strings.TrimSpace(validationPart)

	_, afterFirstQuote, ok := strings.Cut(validationPart, "'")
	if !ok {
		return "invalid parameter"
	}
	field, _, ok := strings.Cut(afterFirstQuote, "'")
	if !ok {
		return "invalid parameter"
	}

	if _, afterTag, ok := strings.Cut(validationPart, "failed on the '"); ok {
		if tag, _, ok := strings.Cut(afterTag, "'"); ok && tag != "" {
			return field + ": " + tag
		}
	}

	return field + ": invalid"
}

// cleanUnmarshalTypeError builds a client-safe message for a JSON
// type-mismatch bind failure from only err.Field (the JSON field name,
// e.g. "username") and err.Type (the Go type it expected, e.g. "string")
// — deliberately never err.Struct or err.Error(), both of which embed
// the Go struct's own name and would leak it exactly like the
// round-2-fixed validator-tag path used to.
func cleanUnmarshalTypeError(err *json.UnmarshalTypeError) string {
	return err.Field + ": expected " + err.Type.String()
}
