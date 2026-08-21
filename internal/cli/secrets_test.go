package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// writeWrappedDEK generates a fresh age identity, wraps a 32-byte DEK under it,
// writes the wrapped blob to a temp file, and returns the age secret key and the
// blob path. It mirrors what `ragctl keys` provisioning will produce.
func writeWrappedDEK(t *testing.T) (ageKey, blobPath string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	dek := bytes.Repeat([]byte{0x7F}, 32)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatalf("age encrypt: %v", err)
	}
	if _, err := w.Write(dek); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	blobPath = filepath.Join(t.TempDir(), "dek.age")
	if err := os.WriteFile(blobPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return id.String(), blobPath
}

// TestLoadStartupCipherMissingDEK proves startup fails closed (a clear error)
// when no wrapped DEK is configured for the local provider (SPEC-09 §2).
func TestLoadStartupCipherMissingDEK(t *testing.T) {
	ageKey, _ := writeWrappedDEK(t)
	_, err := LoadStartupCipher(context.Background(), StartupSecrets{
		KMSProvider:  "local",
		AgeSecretKey: ageKey,
		// DEKWrappedPath deliberately empty: no DEK on disk.
		DEKKeyVersion: 1,
	})
	if err == nil {
		t.Fatal("LoadStartupCipher accepted a missing DEK; want fail-closed error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "dek") {
		t.Fatalf("error %q should mention the DEK", err.Error())
	}
}

// TestLoadStartupCipherSucceeds proves a valid local(age) DEK yields a working
// Cipher at the configured version.
func TestLoadStartupCipherSucceeds(t *testing.T) {
	ageKey, blob := writeWrappedDEK(t)
	c, err := LoadStartupCipher(context.Background(), StartupSecrets{
		KMSProvider:    "local",
		AgeSecretKey:   ageKey,
		DEKWrappedPath: blob,
		DEKKeyVersion:  4,
	})
	if err != nil {
		t.Fatalf("LoadStartupCipher: %v", err)
	}
	ct, err := c.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := c.Decrypt(ct)
	if err != nil || string(pt) != "secret" {
		t.Fatalf("round trip failed: pt=%q err=%v", pt, err)
	}
}

// TestLoadStartupCipherUnknownProvider rejects an unrecognised KMS provider
// rather than silently starting without encryption.
func TestLoadStartupCipherUnknownProvider(t *testing.T) {
	_, err := LoadStartupCipher(context.Background(), StartupSecrets{
		KMSProvider:   "sekret-vault",
		DEKKeyVersion: 1,
	})
	if err == nil {
		t.Fatal("LoadStartupCipher accepted an unknown KMS provider")
	}
}

// TestServeFailsClosedWithoutDEK proves the serve command refuses to start when
// the DEK cannot be loaded, and that the failure is a real error (not the
// not-implemented stub sentinel).
func TestServeFailsClosedWithoutDEK(t *testing.T) {
	// Clear any DEK config from the ambient environment so the case is
	// deterministic (empty is treated as unset; t.Setenv restores afterwards).
	for _, k := range []string{"KMS_PROVIDER", "AGE_SECRET_KEY", "DEK_WRAPPED_PATH", "DEK_KEY_VERSION"} {
		t.Setenv(k, "")
	}
	var stdout, stderr bytes.Buffer
	err := Run([]string{"serve"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("serve started without a DEK; want fail-closed error")
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Fatal("missing-DEK startup failure must be a real error, not ErrNotImplemented")
	}
}
