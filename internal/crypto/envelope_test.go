package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"testing"
)

// testDEK is a fixed 32-byte AES-256 key used across the deterministic vector
// tests below.
func testDEK(t *testing.T) []byte {
	t.Helper()
	k, err := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	return k
}

// TestKnownAnswerVector proves the ciphertext layout is exactly
// [version:2][nonce:12][gcm-output] over AES-256-GCM by reconstructing the
// expected bytes independently and comparing against Cipher output for a fixed
// nonce injected through the test seam.
func TestKnownAnswerVector(t *testing.T) {
	key := testDEK(t)
	c, err := NewCipher(7, key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	nonce, _ := hex.DecodeString("aabbccddeeff00112233445566")
	nonce = nonce[:NonceSize]
	c.randNonce = func(b []byte) error { copy(b, nonce); return nil }

	plaintext := []byte("provider-api-key-value")
	got, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Independently compute the expected ciphertext.
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	want := append([]byte{0x00, 0x07}, nonce...)
	want = append(want, sealed...)

	if !bytes.Equal(got, want) {
		t.Fatalf("ciphertext layout mismatch\n got %x\nwant %x", got, want)
	}
}

// TestRoundTrip proves Encrypt then Decrypt returns the original plaintext with
// a real random nonce.
func TestRoundTrip(t *testing.T) {
	c, err := NewCipher(1, testDEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	plaintext := []byte("s3cr3t-database-password")
	ct, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := c.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("round trip mismatch: got %q want %q", pt, plaintext)
	}
}

// TestTamperDetected proves a single flipped ciphertext byte fails GCM
// authentication rather than returning corrupted plaintext.
func TestTamperDetected(t *testing.T) {
	c, err := NewCipher(1, testDEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	ct, err := c.Encrypt([]byte("do-not-tamper"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip a byte inside the GCM output (past the version+nonce header).
	ct[len(ct)-1] ^= 0x01
	if _, err := c.Decrypt(ct); err == nil {
		t.Fatal("Decrypt accepted tampered ciphertext; want auth failure")
	}
}

// TestNonceUniqueness proves successive encryptions use distinct nonces so a
// key is never reused with the same nonce (a GCM catastrophe).
func TestNonceUniqueness(t *testing.T) {
	c, err := NewCipher(1, testDEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		ct, err := c.Encrypt([]byte("x"))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		nonce := string(ct[VersionSize : VersionSize+NonceSize])
		if seen[nonce] {
			t.Fatalf("nonce reused after %d encryptions", i)
		}
		seen[nonce] = true
	}
}

// TestKeyVersionExposed proves the key version is recoverable from ciphertext
// so rotation (STORY-10.4) can find rows encrypted under an old key without
// decrypting them.
func TestKeyVersionExposed(t *testing.T) {
	c, err := NewCipher(42, testDEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	ct, err := c.Encrypt([]byte("v"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if v := KeyVersion(ct); v != 42 {
		t.Fatalf("KeyVersion = %d, want 42", v)
	}
}

// TestWrongKeySize rejects a DEK that is not 32 bytes (AES-256).
func TestWrongKeySize(t *testing.T) {
	if _, err := NewCipher(1, make([]byte, 16)); err == nil {
		t.Fatal("NewCipher accepted a 16-byte key; want AES-256 (32-byte) requirement")
	}
}

// TestDecryptShortCiphertext rejects input too short to contain the header.
func TestDecryptShortCiphertext(t *testing.T) {
	c, _ := NewCipher(1, testDEK(t))
	if _, err := c.Decrypt([]byte{0x00}); err == nil {
		t.Fatal("Decrypt accepted a truncated ciphertext")
	}
}
