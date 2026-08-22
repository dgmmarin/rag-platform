// Package auth implements control-plane user authentication and sessions
// (STORY-03.1, FR-ACC-01, SPEC-09 §3, SPEC-02 §2/3): argon2id password hashing,
// email/password signup and login, a server-side Postgres session store with a
// hashed cookie id and CSRF double-submit token, and the account lockout policy
// (10 failures / 15 min). It lives in the control plane only (C-3) and never
// logs or returns a password, session token, or CSRF secret.
//
// The HTTP middleware/handlers built on this service (middleware.go) are wired
// into the public router in STORY-04.1 (EPIC-04); until then they are exercised
// with net/http/httptest.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (SPEC-09 §3). These follow the OWASP argon2id guidance and
// are encoded into every hash's PHC string, so raising them later still verifies
// old hashes. m is in KiB.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrInvalidHash tags an encoded hash that is not a well-formed argon2id PHC
// string, so callers can distinguish a corrupt stored hash from a wrong password.
var ErrInvalidHash = errors.New("auth: invalid password hash")

// HashPassword hashes a plaintext password with argon2id and returns the
// standard PHC-format encoded string ("$argon2id$v=19$m=...,t=...,p=...$salt$hash").
// The salt is random per call, so identical passwords produce distinct hashes.
func HashPassword(plaintext string) (string, error) {
	if plaintext == "" {
		return "", errors.New("auth: password must not be empty")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether plaintext matches the argon2id PHC-encoded hash.
// It recomputes the key with the parameters embedded in the hash and compares in
// constant time. A malformed encoded hash returns (false, ErrInvalidHash).
func VerifyPassword(plaintext, encoded string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(plaintext), salt, params.time, params.memory, params.threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

// decodeHash parses a "$argon2id$v=19$m=..,t=..,p=..$salt$hash" PHC string.
func decodeHash(encoded string) (argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// Leading empty element from the initial '$': ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	hash, err := b64.DecodeString(parts[5])
	if err != nil {
		return argonParams{}, nil, nil, ErrInvalidHash
	}
	return p, salt, hash, nil
}
