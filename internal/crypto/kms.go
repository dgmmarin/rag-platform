package crypto

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// KMS wraps and unwraps the data-encryption key (DEK). The DEK is generated
// once and stored wrapped; the KMS key (age identity for self-hosted, an AWS
// KMS key for cloud) is held outside the database (C-4, SPEC-09 §2). Wrap and
// Unwrap are the only operations the platform needs at runtime.
type KMS interface {
	// Wrap encrypts a DEK under the KMS key. Used by provisioning tooling.
	Wrap(ctx context.Context, dek []byte) ([]byte, error)
	// Unwrap decrypts a wrapped DEK. Used at startup (LoadDEK).
	Unwrap(ctx context.Context, wrapped []byte) ([]byte, error)
}

// LocalKMS wraps the DEK with an age X25519 identity for self-hosted
// deployments (ADR-0011): the identity is the KMS key and lives outside the
// database (e.g. a mounted secret file), never in Postgres.
type LocalKMS struct {
	identity  *age.X25519Identity
	recipient *age.X25519Recipient
}

// NewLocalKMS builds a LocalKMS from an age secret key string
// ("AGE-SECRET-KEY-1..."). The identity both wraps (via its recipient) and
// unwraps the DEK.
func NewLocalKMS(ageSecretKey string) (*LocalKMS, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(ageSecretKey))
	if err != nil {
		return nil, fmt.Errorf("crypto: parse age identity: %w", err)
	}
	return &LocalKMS{identity: id, recipient: id.Recipient()}, nil
}

// Wrap encrypts the DEK to the age recipient.
func (k *LocalKMS) Wrap(_ context.Context, dek []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, k.recipient)
	if err != nil {
		return nil, fmt.Errorf("crypto: age encrypt: %w", err)
	}
	if _, err := w.Write(dek); err != nil {
		return nil, fmt.Errorf("crypto: age write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("crypto: age close: %w", err)
	}
	return buf.Bytes(), nil
}

// Unwrap decrypts a DEK wrapped by Wrap. A wrong identity (or tampered blob)
// fails rather than returning garbage.
func (k *LocalKMS) Unwrap(_ context.Context, wrapped []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(wrapped), k.identity)
	if err != nil {
		return nil, fmt.Errorf("crypto: age decrypt: %w", err)
	}
	dek, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("crypto: age read: %w", err)
	}
	return dek, nil
}

// LoadDEK unwraps the wrapped DEK via the configured KMS at startup and returns
// a Cipher bound to keyVersion. It fails closed: a missing wrapped DEK, a KMS
// unwrap failure, or an unwrapped key of the wrong length all return an error
// so the process refuses to start without a usable DEK (SPEC-09 §2).
func LoadDEK(ctx context.Context, kms KMS, wrapped []byte, keyVersion uint16) (*Cipher, error) {
	if len(wrapped) == 0 {
		return nil, fmt.Errorf("crypto: no wrapped DEK configured (cannot start without a data-encryption key)")
	}
	dek, err := kms.Unwrap(ctx, wrapped)
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap DEK: %w", err)
	}
	cipher, err := NewCipher(keyVersion, dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: load DEK: %w", err)
	}
	return cipher, nil
}
