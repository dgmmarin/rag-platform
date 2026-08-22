package provision

import (
	"context"
	"errors"
	"testing"
)

// TestMoveRequiresPrivilegedURL proves Move fails closed with no privileged
// connection configured, before touching a database (mirrors the other
// lifecycle operations).
func TestMoveRequiresPrivilegedURL(t *testing.T) {
	l := &Lifecycle{}
	if _, err := l.Move(context.Background(), "acme", MoveParams{Host: "pg-2"}); !errors.Is(err, errValidation) {
		t.Fatalf("Move with no privileged URL: want errValidation, got %v", err)
	}
}

// TestMoveRejectsEmptySlug proves a blank slug is rejected up front.
func TestMoveRejectsEmptySlug(t *testing.T) {
	l := &Lifecycle{PrivilegedURL: "postgres://x", Encrypter: stubEncrypter{}}
	if _, err := l.Move(context.Background(), "  ", MoveParams{Host: "pg-2"}); !errors.Is(err, errValidation) {
		t.Fatalf("blank slug: want errValidation, got %v", err)
	}
}

// TestMoveRejectsEmptyParams proves a move that changes nothing is rejected: an
// operator must supply at least one connection field, so the command cannot
// silently issue an empty update (fail closed, mirrors validateTransition's
// no-op rejection).
func TestMoveRejectsEmptyParams(t *testing.T) {
	l := &Lifecycle{PrivilegedURL: "postgres://x", Encrypter: stubEncrypter{}}
	if _, err := l.Move(context.Background(), "acme", MoveParams{}); !errors.Is(err, errValidation) {
		t.Fatalf("empty MoveParams: want errValidation, got %v", err)
	}
}

// TestMoveRequiresEncrypterWhenPasswordGiven proves that supplying a new
// password without an Encrypter fails closed: the password must be
// envelope-encrypted with the same DEK the resolver decrypts with (SPEC-09 §2,
// C-4), so a move that would write plaintext-less-than-encrypted is refused.
func TestMoveRequiresEncrypterWhenPasswordGiven(t *testing.T) {
	l := &Lifecycle{PrivilegedURL: "postgres://x"} // no Encrypter
	if _, err := l.Move(context.Background(), "acme", MoveParams{Password: "new-secret"}); !errors.Is(err, errValidation) {
		t.Fatalf("password without encrypter: want errValidation, got %v", err)
	}
}

// TestMoveRejectsNegativePort proves an obviously-wrong port is rejected.
func TestMoveRejectsNegativePort(t *testing.T) {
	l := &Lifecycle{PrivilegedURL: "postgres://x", Encrypter: stubEncrypter{}}
	if _, err := l.Move(context.Background(), "acme", MoveParams{Port: -1}); !errors.Is(err, errValidation) {
		t.Fatalf("negative port: want errValidation, got %v", err)
	}
}

// stubEncrypter is a no-op Encrypter for validation-only unit tests: it never
// reaches a database, so the returned ciphertext is irrelevant.
type stubEncrypter struct{}

func (stubEncrypter) Encrypt(plaintext []byte) ([]byte, error) { return plaintext, nil }
