package crypto

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// fakeKMSAPI is a stand-in for the AWS KMS client so the AWSKMS wrapper can be
// tested without cloud credentials. It records the KeyId and simulates KMS by
// prefixing the plaintext.
type fakeKMSAPI struct {
	sawKeyID   string
	encryptErr error
	decryptErr error
}

func (f *fakeKMSAPI) Encrypt(_ context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	if f.encryptErr != nil {
		return nil, f.encryptErr
	}
	f.sawKeyID = aws.ToString(in.KeyId)
	return &kms.EncryptOutput{CiphertextBlob: append([]byte("kms:"), in.Plaintext...)}, nil
}

func (f *fakeKMSAPI) Decrypt(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	if f.decryptErr != nil {
		return nil, f.decryptErr
	}
	return &kms.DecryptOutput{Plaintext: bytes.TrimPrefix(in.CiphertextBlob, []byte("kms:"))}, nil
}

// TestAWSKMSWrapPassesKeyID proves Wrap forwards the configured key id to the
// KMS Encrypt call and returns the ciphertext blob.
func TestAWSKMSWrapPassesKeyID(t *testing.T) {
	api := &fakeKMSAPI{}
	k := NewAWSKMSWithClient(api, "arn:aws:kms:key/abc")

	dek := bytes.Repeat([]byte{0x11}, KeySize)
	wrapped, err := k.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if api.sawKeyID != "arn:aws:kms:key/abc" {
		t.Fatalf("Encrypt got KeyId %q, want the configured ARN", api.sawKeyID)
	}
	if !bytes.HasPrefix(wrapped, []byte("kms:")) {
		t.Fatalf("Wrap did not return the KMS ciphertext blob: %x", wrapped)
	}
}

// TestAWSKMSRoundTrip proves Wrap then Unwrap returns the original DEK through
// the (faked) KMS boundary.
func TestAWSKMSRoundTrip(t *testing.T) {
	k := NewAWSKMSWithClient(&fakeKMSAPI{}, "arn:key")
	dek := bytes.Repeat([]byte{0x22}, KeySize)
	wrapped, err := k.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	got, err := k.Unwrap(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatalf("round trip mismatch: %x != %x", got, dek)
	}
}

// TestAWSKMSDecryptError proves a KMS failure surfaces as an error (fail
// closed), not a silent empty DEK.
func TestAWSKMSDecryptError(t *testing.T) {
	k := NewAWSKMSWithClient(&fakeKMSAPI{decryptErr: errors.New("access denied")}, "arn:key")
	if _, err := k.Unwrap(context.Background(), []byte("kms:whatever")); err == nil {
		t.Fatal("Unwrap ignored a KMS error")
	}
}
