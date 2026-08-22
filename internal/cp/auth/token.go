package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// sessionTokenBytes is the raw entropy of a session cookie id: 128 bits
// (SPEC-09 §3).
const sessionTokenBytes = 16

// csrfTokenBytes is the raw entropy of a CSRF double-submit token.
const csrfTokenBytes = 32

// newSessionToken returns a URL-safe, unpadded base64 encoding of 16 random
// bytes — the value placed in the session cookie. The plaintext token is never
// stored; only its sha256 (hashToken) is persisted.
func newSessionToken() (string, error) {
	return randomToken(sessionTokenBytes)
}

// newCSRFToken returns a random CSRF token for the double-submit check.
func newCSRFToken() (string, error) {
	return randomToken(csrfTokenBytes)
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken returns the sha256 of a session token, as stored in
// sessions.token_hash. Lookup hashes the presented cookie and matches on this,
// so a leaked control-plane row cannot be replayed as a cookie.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// csrfMatch reports whether two CSRF tokens are equal in constant time. Two
// empty tokens never match, so a missing token cannot pass the double-submit
// check by comparing empty-to-empty.
func csrfMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
