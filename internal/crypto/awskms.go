package crypto

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// kmsAPI is the slice of the AWS KMS client the DEK wrapper needs. Narrowing it
// to two methods lets AWSKMS be unit-tested with a fake, no cloud credentials
// required.
type kmsAPI interface {
	Encrypt(ctx context.Context, in *kms.EncryptInput, optFns ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(ctx context.Context, in *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// AWSKMS wraps the DEK with an AWS KMS customer master key (SPEC-09 §2). The
// KMS key never leaves AWS; only the wrapped DEK is stored.
type AWSKMS struct {
	client kmsAPI
	keyID  string
}

// NewAWSKMS builds an AWSKMS from ambient AWS configuration (env, shared config,
// or instance role). keyID is the KMS key id/ARN used to wrap the DEK.
func NewAWSKMS(ctx context.Context, keyID string) (*AWSKMS, error) {
	if keyID == "" {
		return nil, fmt.Errorf("crypto: AWS KMS key id is required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("crypto: load AWS config: %w", err)
	}
	return &AWSKMS{client: kms.NewFromConfig(cfg), keyID: keyID}, nil
}

// NewAWSKMSWithClient builds an AWSKMS over a supplied client. It is the seam
// used by tests and by callers that already hold a configured KMS client.
func NewAWSKMSWithClient(client kmsAPI, keyID string) *AWSKMS {
	return &AWSKMS{client: client, keyID: keyID}
}

// Wrap encrypts the DEK with the KMS key.
func (k *AWSKMS) Wrap(ctx context.Context, dek []byte) ([]byte, error) {
	out, err := k.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     aws.String(k.keyID),
		Plaintext: dek,
	})
	if err != nil {
		return nil, fmt.Errorf("crypto: kms encrypt: %w", err)
	}
	return out.CiphertextBlob, nil
}

// Unwrap decrypts a wrapped DEK. KMS identifies the key from the ciphertext, so
// no key id is needed here.
func (k *AWSKMS) Unwrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	out, err := k.client.Decrypt(ctx, &kms.DecryptInput{CiphertextBlob: wrapped})
	if err != nil {
		return nil, fmt.Errorf("crypto: kms decrypt: %w", err)
	}
	return out.Plaintext, nil
}
