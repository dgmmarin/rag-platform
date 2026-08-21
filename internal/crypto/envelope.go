// Package crypto implements the envelope encryption used for platform secrets
// (SPEC-09 §2, C-4). A data-encryption key (DEK) encrypts secret columns
// (tenant_databases.password_enc, sources.credentials_enc, provider keys) with
// AES-256-GCM; the DEK itself is wrapped by an external KMS key (see kms.go).
//
// Ciphertext layout (so DEK rotation in STORY-10.4 can find rows under an old
// key without decrypting them):
//
//	[ version : 2 bytes, big-endian ][ nonce : 12 bytes ][ AES-256-GCM output ]
//
// The version identifies which DEK generation sealed the row. The nonce is
// per-message and random; GCM's authentication tag is appended by Seal inside
// the trailing output. Plaintext secrets live in memory only for the duration
// of use and are never logged (SPEC-09 §2).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
)

const (
	// KeySize is the AES-256 key length in bytes.
	KeySize = 32
	// VersionSize is the width of the big-endian key-version header.
	VersionSize = 2
	// NonceSize is the AES-GCM standard nonce length.
	NonceSize = 12
)

// Cipher seals and opens secrets with a single DEK generation identified by
// version. Construct one per active DEK; rotation keeps old-version Ciphers
// around to decrypt not-yet-re-encrypted rows.
type Cipher struct {
	version uint16
	gcm     cipher.AEAD
	// randNonce fills b with a fresh nonce. It is a seam so vector tests can
	// pin the nonce; production uses crypto/rand.
	randNonce func(b []byte) error
}

// NewCipher builds a Cipher for the given key version over the 32-byte DEK.
func NewCipher(version uint16, dek []byte) (*Cipher, error) {
	if len(dek) != KeySize {
		return nil, fmt.Errorf("crypto: DEK must be %d bytes (AES-256), got %d", KeySize, len(dek))
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("crypto: new AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new GCM: %w", err)
	}
	if gcm.NonceSize() != NonceSize {
		return nil, fmt.Errorf("crypto: unexpected GCM nonce size %d", gcm.NonceSize())
	}
	return &Cipher{
		version:   version,
		gcm:       gcm,
		randNonce: func(b []byte) error { _, err := rand.Read(b); return err },
	}, nil
}

// Encrypt seals plaintext, returning version || nonce || gcm-output.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, NonceSize)
	if err := c.randNonce(nonce); err != nil {
		return nil, fmt.Errorf("crypto: read nonce: %w", err)
	}
	out := make([]byte, VersionSize+NonceSize, VersionSize+NonceSize+len(plaintext)+c.gcm.Overhead())
	binary.BigEndian.PutUint16(out[:VersionSize], c.version)
	copy(out[VersionSize:], nonce)
	return c.gcm.Seal(out, nonce, plaintext, nil), nil
}

// Decrypt opens a ciphertext produced by a Cipher of the same key version. It
// returns an error on any authentication failure (tamper) or malformed input.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < VersionSize+NonceSize {
		return nil, fmt.Errorf("crypto: ciphertext too short (%d bytes)", len(ciphertext))
	}
	if v := KeyVersion(ciphertext); v != c.version {
		return nil, fmt.Errorf("crypto: key version mismatch: ciphertext v%d, cipher v%d", v, c.version)
	}
	nonce := ciphertext[VersionSize : VersionSize+NonceSize]
	sealed := ciphertext[VersionSize+NonceSize:]
	pt, err := c.gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return pt, nil
}

// KeyVersion reads the key-version header from a ciphertext without decrypting
// it, so rotation can select rows sealed under an old DEK.
func KeyVersion(ciphertext []byte) uint16 {
	if len(ciphertext) < VersionSize {
		return 0
	}
	return binary.BigEndian.Uint16(ciphertext[:VersionSize])
}
