package auth

import (
	"strings"
	"testing"
)

// TestHashPasswordRoundTrips proves a hashed password verifies against the
// original plaintext and rejects a wrong one (SPEC-09 §3: argon2id).
func TestHashPasswordRoundTrips(t *testing.T) {
	const pw = "correct horse battery staple"
	encoded, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	// The encoded hash must be the standard argon2id PHC string and must never
	// contain the plaintext.
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("encoded hash is not argon2id PHC format: %q", encoded)
	}
	if strings.Contains(encoded, pw) {
		t.Fatal("encoded hash leaked the plaintext password")
	}

	ok, err := VerifyPassword(pw, encoded)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("VerifyPassword rejected the correct password")
	}

	bad, err := VerifyPassword("wrong password", encoded)
	if err != nil {
		t.Fatalf("VerifyPassword (wrong): %v", err)
	}
	if bad {
		t.Fatal("VerifyPassword accepted a wrong password")
	}
}

// TestHashPasswordSaltsAreUnique proves two hashes of the same password differ
// (per-hash random salt), so identical passwords are not detectable in storage.
func TestHashPasswordSaltsAreUnique(t *testing.T) {
	a, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword a: %v", err)
	}
	b, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword b: %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical; salt is not random")
	}
}

// TestHashPasswordRejectsEmpty guards the trivial footgun of hashing an empty
// password.
func TestHashPasswordRejectsEmpty(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Fatal("HashPassword accepted an empty password")
	}
}

// TestVerifyPasswordRejectsMalformed proves a corrupt/foreign encoded hash is a
// verification failure with an error, not a panic or a silent accept.
func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	for _, enc := range []string{
		"",
		"not-a-phc-string",
		"$argon2id$v=19$m=65536", // truncated
		"$bcrypt$whatever",       // wrong algorithm
	} {
		ok, err := VerifyPassword("x", enc)
		if ok {
			t.Fatalf("VerifyPassword accepted malformed hash %q", enc)
		}
		if err == nil {
			t.Fatalf("VerifyPassword returned no error for malformed hash %q", enc)
		}
	}
}
