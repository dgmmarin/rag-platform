package cli

import (
	"context"
	"fmt"
	"os"

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
}

// startupSecretsFromConfig maps a loaded Config to StartupSecrets.
func startupSecretsFromConfig(cfg config.Config) StartupSecrets {
	return StartupSecrets{
		KMSProvider:    cfg.KMSProvider,
		AgeSecretKey:   cfg.AgeSecretKey,
		AWSKMSKeyID:    cfg.AWSKMSKeyID,
		DEKWrappedPath: cfg.DEKWrappedPath,
		DEKKeyVersion:  cfg.DEKKeyVersion,
	}
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
