package crypto

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"filippo.io/age"
)

// newTestAgeIdentity generates a fresh age X25519 secret-key string for use as
// a local-KMS key in tests.
func newTestAgeIdentity(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return id.String()
}

// TestLocalKMSRoundTrip proves the age-based local KMS can wrap a DEK and
// unwrap it back to the same bytes, using an age identity as the KMS key.
func TestLocalKMSRoundTrip(t *testing.T) {
	identity := newTestAgeIdentity(t)
	kms, err := NewLocalKMS(identity)
	if err != nil {
		t.Fatalf("NewLocalKMS: %v", err)
	}

	dek := bytes.Repeat([]byte{0xAB}, KeySize)
	wrapped, err := kms.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if bytes.Equal(wrapped, dek) {
		t.Fatal("wrapped DEK equals plaintext DEK; not encrypted")
	}

	unwrapped, err := kms.Unwrap(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(unwrapped, dek) {
		t.Fatalf("unwrap mismatch: got %x want %x", unwrapped, dek)
	}
}

// TestLocalKMSWrongIdentity proves a DEK wrapped for one identity cannot be
// unwrapped by a different one (fail closed).
func TestLocalKMSWrongIdentity(t *testing.T) {
	kms1, _ := NewLocalKMS(newTestAgeIdentity(t))
	kms2, _ := NewLocalKMS(newTestAgeIdentity(t))

	wrapped, err := kms1.Wrap(context.Background(), bytes.Repeat([]byte{0x01}, KeySize))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if _, err := kms2.Unwrap(context.Background(), wrapped); err == nil {
		t.Fatal("Unwrap succeeded with the wrong identity; want failure")
	}
}

// fakeKMS is an in-memory KMS used to test LoadDEK's startup wiring without any
// cloud or key material on disk.
type fakeKMS struct {
	unwrap func(ctx context.Context, wrapped []byte) ([]byte, error)
}

func (f fakeKMS) Wrap(_ context.Context, dek []byte) ([]byte, error) {
	return append([]byte("wrapped:"), dek...), nil
}

func (f fakeKMS) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	return f.unwrap(ctx, wrapped)
}

// TestLoadDEKMissing proves startup fails closed when no wrapped DEK is
// configured (SPEC-09 §2: a missing DEK must fail startup).
func TestLoadDEKMissing(t *testing.T) {
	kms := fakeKMS{unwrap: func(context.Context, []byte) ([]byte, error) {
		t.Fatal("Unwrap should not be called when the wrapped DEK is absent")
		return nil, nil
	}}
	if _, err := LoadDEK(context.Background(), kms, nil, 1); err == nil {
		t.Fatal("LoadDEK accepted a missing wrapped DEK; want fail-closed error")
	}
}

// TestLoadDEKUnwrapFails proves a KMS unwrap failure propagates as a startup
// error rather than a silent empty key.
func TestLoadDEKUnwrapFails(t *testing.T) {
	kms := fakeKMS{unwrap: func(context.Context, []byte) ([]byte, error) {
		return nil, errors.New("kms boom")
	}}
	if _, err := LoadDEK(context.Background(), kms, []byte("something"), 1); err == nil {
		t.Fatal("LoadDEK ignored an unwrap failure; want error")
	}
}

// TestLoadDEKSuccess proves a valid wrapped DEK yields a working Cipher at the
// configured key version.
func TestLoadDEKSuccess(t *testing.T) {
	dek := bytes.Repeat([]byte{0x5A}, KeySize)
	kms := fakeKMS{unwrap: func(context.Context, []byte) ([]byte, error) { return dek, nil }}

	c, err := LoadDEK(context.Background(), kms, []byte("wrapped-blob"), 3)
	if err != nil {
		t.Fatalf("LoadDEK: %v", err)
	}
	ct, err := c.Encrypt([]byte("hi"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if KeyVersion(ct) != 3 {
		t.Fatalf("cipher used version %d, want 3", KeyVersion(ct))
	}
}

// TestLoadDEKWrongSize proves an unwrapped DEK of the wrong length is rejected
// (fail closed rather than build a broken cipher).
func TestLoadDEKWrongSize(t *testing.T) {
	kms := fakeKMS{unwrap: func(context.Context, []byte) ([]byte, error) {
		return []byte("too-short"), nil
	}}
	if _, err := LoadDEK(context.Background(), kms, []byte("w"), 1); err == nil {
		t.Fatal("LoadDEK accepted a wrong-size DEK")
	}
}
