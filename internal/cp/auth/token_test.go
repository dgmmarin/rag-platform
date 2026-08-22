package auth

import (
	"encoding/base64"
	"testing"
)

// TestNewSessionTokenIs128BitRandom proves the cookie token is a URL-safe
// encoding of 16 random bytes (128-bit, SPEC-09 §3) and is unique per call.
func TestNewSessionTokenIs128BitRandom(t *testing.T) {
	a, err := newSessionToken()
	if err != nil {
		t.Fatalf("newSessionToken: %v", err)
	}
	b, err := newSessionToken()
	if err != nil {
		t.Fatalf("newSessionToken: %v", err)
	}
	if a == b {
		t.Fatal("two session tokens collided; not random")
	}
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil {
		t.Fatalf("token is not raw-url base64: %v", err)
	}
	if len(raw) != 16 {
		t.Fatalf("token decodes to %d bytes, want 16 (128-bit)", len(raw))
	}
}

// TestHashTokenIsStableAndOpaque proves the stored token hash is deterministic
// for a token (so lookup works) but is not the token itself (so a leaked DB row
// cannot be replayed as a cookie).
func TestHashTokenIsStableAndOpaque(t *testing.T) {
	tok, err := newSessionToken()
	if err != nil {
		t.Fatalf("newSessionToken: %v", err)
	}
	h1 := hashToken(tok)
	h2 := hashToken(tok)
	if len(h1) != 32 {
		t.Fatalf("token hash is %d bytes, want 32 (sha256)", len(h1))
	}
	if string(h1) != string(h2) {
		t.Fatal("hashToken is not deterministic")
	}
	if string(h1) == tok {
		t.Fatal("token hash equals the token")
	}
	if string(h1) == string(hashToken(tok+"x")) {
		t.Fatal("distinct tokens hash to the same value")
	}
}

// TestNewCSRFTokenRandom proves CSRF tokens are random and non-empty.
func TestNewCSRFTokenRandom(t *testing.T) {
	a, err := newCSRFToken()
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}
	b, err := newCSRFToken()
	if err != nil {
		t.Fatalf("newCSRFToken: %v", err)
	}
	if a == "" || a == b {
		t.Fatalf("CSRF tokens not random/non-empty: %q %q", a, b)
	}
}

// TestCSRFMatchConstantTime proves the double-submit comparison accepts equal
// tokens and rejects unequal ones (including empty).
func TestCSRFMatchConstantTime(t *testing.T) {
	tok, _ := newCSRFToken()
	if !csrfMatch(tok, tok) {
		t.Fatal("csrfMatch rejected identical tokens")
	}
	if csrfMatch(tok, tok+"x") {
		t.Fatal("csrfMatch accepted differing tokens")
	}
	if csrfMatch("", "") {
		t.Fatal("csrfMatch accepted two empty tokens")
	}
}
