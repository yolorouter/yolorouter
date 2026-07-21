// Package service is the business-logic layer for the auth module —
// password/session-token helpers and the six admin-auth operations,
// calling internal/repository for all data access.
package service

import "golang.org/x/crypto/bcrypt"

// HashPassword hashes a plaintext password with bcrypt at the library's
// default cost — the same algorithm the reference project's non-admin
// password-login path uses.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// CheckPassword reports whether password matches hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
