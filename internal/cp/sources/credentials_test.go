package sources

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeCipher is a reversible stand-in for *crypto.Cipher in the service tests. It
// XORs every byte with 0xAA, so ciphertext is never equal to a non-trivial
// plaintext (enough to prove "stored credentials are not the plaintext") and
// round-trips through Decrypt. The real envelope crypto is exercised end to end
// in test/e2e/credentials_e2e_test.go.
type fakeCipher struct{ failEncrypt, failDecrypt bool }

func (c fakeCipher) Encrypt(plaintext []byte) ([]byte, error) {
	if c.failEncrypt {
		return nil, errors.New("boom")
	}
	return xor(plaintext), nil
}

func (c fakeCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if c.failDecrypt {
		return nil, errors.New("boom")
	}
	return xor(ciphertext), nil
}

func xor(b []byte) []byte {
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = v ^ 0xAA
	}
	return out
}

// TestCreateEncryptsCredentials proves the write path seals credentials with the
// Encrypter, stores the CIPHERTEXT (never the plaintext), and returns a source
// that carries no credentials (FR-SRC-10, SPEC-04 §6).
func TestCreateEncryptsCredentials(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	svc.Encrypter = fakeCipher{}

	src, err := svc.Create(context.Background(), CreateParams{
		TenantID:    tenantA,
		Kind:        "api",
		Name:        "n",
		Credentials: map[string]string{"token": "super-secret"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	enc, err := st.GetCredentials(context.Background(), tenantA, src.ID)
	if err != nil {
		t.Fatalf("get credentials: %v", err)
	}
	if len(enc) == 0 {
		t.Fatal("no credentials stored")
	}
	if strings.Contains(string(enc), "super-secret") {
		t.Fatalf("stored credentials are plaintext, not ciphertext: %q", enc)
	}
	// Decrypting the stored bytes recovers the original JSON.
	pt, _ := fakeCipher{}.Decrypt(enc)
	if !strings.Contains(string(pt), "super-secret") {
		t.Fatalf("ciphertext does not round-trip: %q", pt)
	}
}

// TestCreateCredentialsRequireEncrypter fails closed: credentials on the write
// path with no Encrypter wired must be an error, never a silent plaintext store.
func TestCreateCredentialsRequireEncrypter(t *testing.T) {
	svc := newTestService(t, newFakeStore())
	_, err := svc.Create(context.Background(), CreateParams{
		TenantID:    tenantA,
		Kind:        "api",
		Name:        "n",
		Credentials: map[string]string{"token": "x"},
	})
	if err == nil {
		t.Fatal("want error when credentials given without an Encrypter (fail closed)")
	}
}

// TestUpdateEncryptsCredentials proves PATCH re-seals replacement credentials.
func TestUpdateEncryptsCredentials(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	svc.Encrypter = fakeCipher{}
	src, err := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "api", Name: "n"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Update(context.Background(), UpdateParams{
		TenantID: tenantA, ID: src.ID,
		Patch: UpdatePatch{Credentials: map[string]string{"token": "rotated-secret"}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	enc, _ := st.GetCredentials(context.Background(), tenantA, src.ID)
	if strings.Contains(string(enc), "rotated-secret") {
		t.Fatalf("updated credentials stored as plaintext: %q", enc)
	}
	pt, _ := fakeCipher{}.Decrypt(enc)
	if !strings.Contains(string(pt), "rotated-secret") {
		t.Fatalf("updated ciphertext does not round-trip: %q", pt)
	}
}

// TestTestConnectionDecryptsCredentials proves the Test seam decrypts the stored
// credentials and hands the plaintext map to the connector (SPEC-04 §6).
func TestTestConnectionDecryptsCredentials(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	svc.Encrypter = fakeCipher{}
	svc.Decrypter = fakeCipher{}
	src, err := svc.Create(context.Background(), CreateParams{
		TenantID:    tenantA,
		Kind:        "api",
		Name:        "n",
		Credentials: map[string]string{"token": "abc123"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var gotToken string
	svc.Validator = fakeValidator{test: func(_ context.Context, _ string, _ json.RawMessage, creds map[string]string) error {
		gotToken = creds["token"] // read during the call, before the service zeroes it
		return nil
	}}
	if err := svc.Test(context.Background(), tenantA, src.ID); err != nil {
		t.Fatalf("test: %v", err)
	}
	if gotToken != "abc123" {
		t.Fatalf("connector did not receive decrypted credentials, got token=%q", gotToken)
	}
}

// TestTestConnectionZeroesCredentialsAfterUse proves the decrypted credential map
// is cleared once Test returns (SPEC-04 §6 "zeroed afterwards"). The connector
// retains the map reference (a misbehaving connector); the service must have
// emptied it by the time Test returns.
func TestTestConnectionZeroesCredentialsAfterUse(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	svc.Encrypter = fakeCipher{}
	svc.Decrypter = fakeCipher{}
	src, _ := svc.Create(context.Background(), CreateParams{
		TenantID: tenantA, Kind: "api", Name: "n",
		Credentials: map[string]string{"token": "abc123"},
	})
	var retained map[string]string
	svc.Validator = fakeValidator{test: func(_ context.Context, _ string, _ json.RawMessage, creds map[string]string) error {
		retained = creds // hold the reference past the call
		return nil
	}}
	if err := svc.Test(context.Background(), tenantA, src.ID); err != nil {
		t.Fatalf("test: %v", err)
	}
	if len(retained) != 0 {
		t.Fatalf("credentials not zeroed after Test: %+v", retained)
	}
}

// TestOpenCredentialsErrorsAreSanitised proves a decrypt failure yields a message
// that carries neither the ciphertext nor a secret value (SPEC-04 §6). A source is
// sealed successfully, then decryption is forced to fail.
func TestOpenCredentialsErrorsAreSanitised(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	svc.Encrypter = fakeCipher{}
	src, err := svc.Create(context.Background(), CreateParams{
		TenantID: tenantA, Kind: "api", Name: "n",
		Credentials: map[string]string{"token": "leak-me"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	svc.Decrypter = fakeCipher{failDecrypt: true}
	svc.Validator = fakeValidator{}
	err = svc.Test(context.Background(), tenantA, src.ID)
	if err == nil {
		t.Fatal("want a decrypt error")
	}
	enc, _ := st.GetCredentials(context.Background(), tenantA, src.ID)
	msg := err.Error()
	if strings.Contains(msg, "leak-me") || strings.Contains(msg, string(enc)) {
		t.Fatalf("error message leaks secret material: %q", msg)
	}
}

// TestTestConnectionNoCredentials proves a source without credentials tests fine
// and the connector receives no credential material.
func TestTestConnectionNoCredentials(t *testing.T) {
	st := newFakeStore()
	svc := newTestService(t, st)
	svc.Decrypter = fakeCipher{}
	src, _ := svc.Create(context.Background(), CreateParams{TenantID: tenantA, Kind: "api", Name: "n"})
	var got map[string]string
	sawNil := false
	svc.Validator = fakeValidator{test: func(_ context.Context, _ string, _ json.RawMessage, creds map[string]string) error {
		got = creds
		sawNil = creds == nil
		return nil
	}}
	if err := svc.Test(context.Background(), tenantA, src.ID); err != nil {
		t.Fatalf("test: %v", err)
	}
	if len(got) != 0 || !sawNil {
		t.Fatalf("expected no credentials, got %+v", got)
	}
}
