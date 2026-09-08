package crypto

import "fmt"

// Keyring holds a PRIMARY Cipher plus any number of PREVIOUS-version Ciphers kept for
// decryption only (STORY-10.4, SPEC-09 §2, NFR-SEC-03). It is the zero-downtime enabler
// for DEK rotation: while `ragctl keys rotate-dek` re-encrypts stored secrets from an
// old version to the new one, the running fleet holds both keys, so any row — already
// migrated or not — decrypts. New secrets are always sealed under the primary version.
//
// A single-key deployment is just a keyring with one primary and no previous keys, so
// Keyring drops in wherever a *Cipher was used (it satisfies the same Encrypt/Decrypt
// interfaces).
type Keyring struct {
	primary   *Cipher
	byVersion map[uint16]*Cipher
}

// NewKeyring builds a keyring whose primary seals new secrets and whose primary plus
// previous keys can all open old ciphertext. A previous key at the primary's version is
// ignored (the primary wins). primary must be non-nil.
func NewKeyring(primary *Cipher, previous ...*Cipher) (*Keyring, error) {
	if primary == nil {
		return nil, fmt.Errorf("crypto: keyring needs a primary cipher")
	}
	kr := &Keyring{primary: primary, byVersion: map[uint16]*Cipher{primary.Version(): primary}}
	for _, c := range previous {
		if c == nil {
			return nil, fmt.Errorf("crypto: nil previous cipher in keyring")
		}
		if _, ok := kr.byVersion[c.Version()]; !ok {
			kr.byVersion[c.Version()] = c
		}
	}
	return kr, nil
}

// Encrypt seals plaintext under the PRIMARY version.
func (kr *Keyring) Encrypt(plaintext []byte) ([]byte, error) { return kr.primary.Encrypt(plaintext) }

// Decrypt opens a ciphertext with whichever key version sealed it, failing closed when
// no key in the ring matches the header version.
func (kr *Keyring) Decrypt(ciphertext []byte) ([]byte, error) {
	v := KeyVersion(ciphertext)
	c, ok := kr.byVersion[v]
	if !ok {
		return nil, fmt.Errorf("crypto: no key for version %d in keyring", v)
	}
	return c.Decrypt(ciphertext)
}

// PrimaryVersion is the version new secrets are sealed under (the rotation target).
func (kr *Keyring) PrimaryVersion() uint16 { return kr.primary.Version() }

// Reencrypt re-seals one stored secret under the primary version. changed is false —
// and field returned unchanged — when the field is already at the primary version or is
// empty, so a rotation sweep is idempotent and resumable. The recovered plaintext is
// zeroed before returning.
func (kr *Keyring) Reencrypt(field []byte) (out []byte, changed bool, err error) {
	if len(field) == 0 || KeyVersion(field) == kr.PrimaryVersion() {
		return field, false, nil
	}
	pt, err := kr.Decrypt(field)
	if err != nil {
		return nil, false, fmt.Errorf("crypto: reencrypt decrypt: %w", err)
	}
	defer Zero(pt)
	out, err = kr.Encrypt(pt)
	if err != nil {
		return nil, false, fmt.Errorf("crypto: reencrypt seal: %w", err)
	}
	return out, true, nil
}
