package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// API-key wire format (SPEC-02 §3, ADR-0021): rk_<prefix>_<secret>.
//   - rk_ is a fixed scheme marker so a leaked key is recognisable in scanners.
//   - <prefix> is 8 hex chars stored in the clear (key_prefix) for indexed
//     lookup and for display. Hex is deliberate: it never contains the '_'
//     segment separator, so the prefix is always recovered intact by SplitN,
//     which base64url (alphabet includes '_') would not guarantee.
//   - <secret> is 32 bytes (SPEC-09 §3) of base64url entropy, shown once. It may
//     contain '_', which is fine: SplitN(value, "_", 3) keeps the tail whole.
//
// Only the sha256 of the FULL presented value (rk_<prefix>_<secret>) is stored
// (key_hash); the plaintext is never persisted (FR-ACC-05, C-4).
const (
	apiKeyScheme    = "rk"
	apiKeyPrefixLen = 8
	// apiKeySecretBytes is the raw entropy of the secret segment (SPEC-09 §3).
	apiKeySecretBytes = 32
	// apiKeyPrefixBytes is the entropy behind the 8-char hex prefix: 4 bytes ->
	// 8 hex chars, so the prefix is exactly apiKeyPrefixLen.
	apiKeyPrefixBytes = apiKeyPrefixLen / 2
)

// newAPIKeySecret mints a fresh key: it returns the full plaintext secret to
// show once, the display/lookup prefix, and the sha256 of the full secret to
// store. It reuses the sha256 token-hashing approach from session tokens so a
// leaked control-plane row cannot be replayed as a key.
func newAPIKeySecret() (secret, prefix string, hash []byte, err error) {
	pb := make([]byte, apiKeyPrefixBytes)
	if _, err = rand.Read(pb); err != nil {
		return "", "", nil, fmt.Errorf("auth: read key prefix entropy: %w", err)
	}
	sb := make([]byte, apiKeySecretBytes)
	if _, err = rand.Read(sb); err != nil {
		return "", "", nil, fmt.Errorf("auth: read key secret entropy: %w", err)
	}
	prefix = hex.EncodeToString(pb) // 4 bytes -> 8 hex chars, no '_'
	body := base64.RawURLEncoding.EncodeToString(sb)
	secret = apiKeyScheme + "_" + prefix + "_" + body
	return secret, prefix, hashToken(secret), nil
}
