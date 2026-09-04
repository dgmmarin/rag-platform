package crypto

// Zero overwrites b in place with zero bytes. Use it to erase decrypted secret
// plaintext the moment it is no longer needed (SPEC-09 §2, SPEC-04 §6: secrets
// are "decrypted into memory only for the duration of use"). Keep decrypted
// secrets in a []byte and Zero it, rather than in a string — a string's backing
// array cannot be overwritten in place.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
