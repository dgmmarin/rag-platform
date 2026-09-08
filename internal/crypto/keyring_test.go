package crypto

import (
	"bytes"
	"testing"
)

func testCipher(t *testing.T, version uint16, fill byte) *Cipher {
	t.Helper()
	c, err := NewCipher(version, bytes.Repeat([]byte{fill}, KeySize))
	if err != nil {
		t.Fatalf("NewCipher v%d: %v", version, err)
	}
	return c
}

// A keyring seals with its primary and opens ciphertext from any key version it holds.
func TestKeyringEncryptsPrimaryDecryptsAnyVersion(t *testing.T) {
	v1 := testCipher(t, 1, 0x11)
	v2 := testCipher(t, 2, 0x22)
	kr, err := NewKeyring(v2, v1)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	if kr.PrimaryVersion() != 2 {
		t.Fatalf("primary version = %d, want 2", kr.PrimaryVersion())
	}

	// New secrets seal under the primary (v2).
	ct, err := kr.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if KeyVersion(ct) != 2 {
		t.Fatalf("sealed under version %d, want 2 (primary)", KeyVersion(ct))
	}

	// Old ciphertext sealed under v1 still opens.
	old, _ := v1.Encrypt([]byte("legacy"))
	pt, err := kr.Decrypt(old)
	if err != nil {
		t.Fatalf("decrypt v1: %v", err)
	}
	if string(pt) != "legacy" {
		t.Fatalf("decrypt v1 = %q, want legacy", pt)
	}

	// A version the ring does not hold fails closed.
	v3 := testCipher(t, 3, 0x33)
	orphan, _ := v3.Encrypt([]byte("x"))
	if _, err := kr.Decrypt(orphan); err == nil {
		t.Fatal("want an error decrypting a version not in the ring")
	}
}

// Reencrypt is idempotent: a field already at the primary version is untouched; an old
// field is re-sealed under the primary and still decrypts to the same plaintext.
func TestKeyringReencryptIsIdempotent(t *testing.T) {
	v1 := testCipher(t, 1, 0x11)
	v2 := testCipher(t, 2, 0x22)
	kr, _ := NewKeyring(v2, v1)

	// An empty field is a no-op.
	if out, changed, err := kr.Reencrypt(nil); err != nil || changed || out != nil {
		t.Fatalf("Reencrypt(nil) = (%v,%v,%v), want (nil,false,nil)", out, changed, err)
	}

	old, _ := v1.Encrypt([]byte("rotate me"))
	out, changed, err := kr.Reencrypt(old)
	if err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	if !changed {
		t.Fatal("a v1 field should be re-encrypted (changed=true)")
	}
	if KeyVersion(out) != 2 {
		t.Fatalf("re-encrypted under version %d, want 2", KeyVersion(out))
	}
	pt, err := kr.Decrypt(out)
	if err != nil || string(pt) != "rotate me" {
		t.Fatalf("decrypt re-encrypted = (%q,%v), want (rotate me,nil)", pt, err)
	}

	// Running again is a no-op — the field is already primary.
	out2, changed2, err := kr.Reencrypt(out)
	if err != nil {
		t.Fatalf("reencrypt idempotent: %v", err)
	}
	if changed2 {
		t.Fatal("re-encrypting an already-primary field must be a no-op (changed=false)")
	}
	if !bytes.Equal(out, out2) {
		t.Fatal("idempotent reencrypt should return the field unchanged")
	}
}
