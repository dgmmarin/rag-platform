package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rag-platform/ragctl/internal/config"
	"github.com/rag-platform/ragctl/internal/crypto"
)

// StartupSecrets is the resolved secret-loading configuration a long-running
// command (serve, work) needs at startup (STORY-01.4, SPEC-09 §2).
type StartupSecrets struct {
	KMSProvider    string
	AgeSecretKey   string
	AWSKMSKeyID    string
	DEKWrappedPath string
	DEKKeyVersion  uint16
	// Previous are earlier DEK generations kept for DECRYPT only during a rotation
	// window (STORY-10.4); empty in steady state.
	Previous []PreviousDEK
}

// PreviousDEK is one earlier DEK generation (wrapped-blob path + its version).
type PreviousDEK struct {
	WrappedPath string
	Version     uint16
}

// startupSecretsFromConfig maps a loaded Config to StartupSecrets.
func startupSecretsFromConfig(cfg config.Config) StartupSecrets {
	s := StartupSecrets{
		KMSProvider:    cfg.KMSProvider,
		AgeSecretKey:   cfg.AgeSecretKey,
		AWSKMSKeyID:    cfg.AWSKMSKeyID,
		DEKWrappedPath: cfg.DEKWrappedPath,
		DEKKeyVersion:  cfg.DEKKeyVersion,
	}
	for _, e := range cfg.DEKPrevious {
		if p, err := parsePreviousDEK(e); err == nil {
			s.Previous = append(s.Previous, p)
		}
	}
	return s
}

// parsePreviousDEK parses a "path:version" DEK_PREVIOUS entry.
func parsePreviousDEK(entry string) (PreviousDEK, error) {
	i := strings.LastIndex(entry, ":")
	if i <= 0 || i == len(entry)-1 {
		return PreviousDEK{}, fmt.Errorf("startup: bad DEK_PREVIOUS entry %q (want path:version)", entry)
	}
	v, err := strconv.ParseUint(entry[i+1:], 10, 16)
	if err != nil {
		return PreviousDEK{}, fmt.Errorf("startup: bad DEK_PREVIOUS version in %q: %w", entry, err)
	}
	return PreviousDEK{WrappedPath: entry[:i], Version: uint16(v)}, nil
}

// LoadStartupCipher builds the configured KMS, reads the wrapped DEK from disk,
// unwraps it, and returns a Cipher bound to the active key version. It fails
// closed: any missing/invalid KMS key, missing DEK blob, or unwrap failure is a
// startup error so the platform never runs without a usable DEK (SPEC-09 §2).
//
// The DEK plaintext lives only inside the returned Cipher; nothing here logs the
// wrapped or unwrapped key material.
func LoadStartupCipher(ctx context.Context, s StartupSecrets) (*crypto.Cipher, error) {
	kms, err := buildKMS(ctx, s)
	if err != nil {
		return nil, err
	}

	wrapped, err := readWrappedDEK(s.DEKWrappedPath)
	if err != nil {
		return nil, err
	}

	cipher, err := crypto.LoadDEK(ctx, kms, wrapped, s.DEKKeyVersion)
	if err != nil {
		return nil, err
	}
	return cipher, nil
}

// LoadStartupKeyring loads the primary DEK (as LoadStartupCipher) plus any previous
// generations still configured for the rotation window, returning a Keyring that seals
// new secrets under the primary and can decrypt any configured version (STORY-10.4,
// SPEC-09 §2). With no previous DEKs it is a single-key keyring, behaviourally identical
// to the primary Cipher.
func LoadStartupKeyring(ctx context.Context, s StartupSecrets) (*crypto.Keyring, error) {
	primary, err := LoadStartupCipher(ctx, s)
	if err != nil {
		return nil, err
	}
	kms, err := buildKMS(ctx, s)
	if err != nil {
		return nil, err
	}
	previous := make([]*crypto.Cipher, 0, len(s.Previous))
	for _, p := range s.Previous {
		wrapped, err := readWrappedDEK(p.WrappedPath)
		if err != nil {
			return nil, err
		}
		c, err := crypto.LoadDEK(ctx, kms, wrapped, p.Version)
		if err != nil {
			return nil, fmt.Errorf("startup: load previous DEK v%d: %w", p.Version, err)
		}
		previous = append(previous, c)
	}
	return crypto.NewKeyring(primary, previous...)
}

// buildKMS constructs the KMS implementation named by the provider.
func buildKMS(ctx context.Context, s StartupSecrets) (crypto.KMS, error) {
	switch s.KMSProvider {
	case "local":
		if s.AgeSecretKey == "" {
			return nil, fmt.Errorf("startup: local KMS selected but AGE_SECRET_KEY is unset")
		}
		return crypto.NewLocalKMS(s.AgeSecretKey)
	case "aws":
		return crypto.NewAWSKMS(ctx, s.AWSKMSKeyID)
	default:
		return nil, fmt.Errorf("startup: unknown KMS provider %q (want local or aws)", s.KMSProvider)
	}
}

// readWrappedDEK reads the wrapped-DEK blob, returning a fail-closed error when
// the path is unset or the file is missing.
func readWrappedDEK(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("startup: no wrapped DEK configured (set DEK_WRAPPED_PATH); cannot start without a data-encryption key")
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("startup: read wrapped DEK: %w", err)
	}
	if len(blob) == 0 {
		return nil, fmt.Errorf("startup: wrapped DEK file %s is empty", path)
	}
	return blob, nil
}
