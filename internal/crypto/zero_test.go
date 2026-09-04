package crypto

import (
	"bytes"
	"testing"
)

// TestZeroOverwritesBuffer proves Zero clears every byte of a decrypted-secret
// buffer, the "decrypted into memory only for the duration of use" hygiene
// SPEC-09 §2 / SPEC-04 §6 require before a plaintext secret goes out of scope.
func TestZeroOverwritesBuffer(t *testing.T) {
	b := []byte("s3cr3t-token-value")
	Zero(b)
	if !bytes.Equal(b, make([]byte, len(b))) {
		t.Fatalf("Zero left non-zero bytes: %v", b)
	}
}

// TestZeroEmptyAndNil is a no-op guard: Zero must not panic on empty or nil.
func TestZeroEmptyAndNil(_ *testing.T) {
	Zero(nil)
	Zero([]byte{})
}
