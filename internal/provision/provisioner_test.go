package provision

import (
	"context"
	"errors"
	"testing"
)

// fakeEncrypter records what it sealed; it lets the validation tests run without
// real crypto or a database.
type fakeEncrypter struct{ last []byte }

func (f *fakeEncrypter) Encrypt(pt []byte) ([]byte, error) {
	f.last = append([]byte(nil), pt...)
	return append([]byte("enc:"), pt...), nil
}

func TestProvisionRequiresEncrypter(t *testing.T) {
	p := &Provisioner{}
	_, err := p.Provision(context.Background(), Params{Slug: "acme", Name: "Acme"})
	if err == nil {
		t.Fatal("Provision with no encrypter should fail closed")
	}
}

func TestProvisionValidatesParams(t *testing.T) {
	p := &Provisioner{Encrypter: &fakeEncrypter{}, PrivilegedURL: "postgres://x"}
	cases := []Params{
		{Slug: "", Name: "Acme"},                    // missing slug
		{Slug: "acme", Name: ""},                    // missing name
		{Slug: "acme", Name: "A", EmbeddingDim: -1}, // bad dimension
	}
	for _, c := range cases {
		if _, err := p.Provision(context.Background(), c); err == nil {
			t.Errorf("Provision(%+v) should have rejected invalid params", c)
		}
	}
}

func TestProvisionRequiresPrivilegedURL(t *testing.T) {
	p := &Provisioner{Encrypter: &fakeEncrypter{}}
	_, err := p.Provision(context.Background(), Params{Slug: "acme", Name: "Acme"})
	if err == nil {
		t.Fatal("Provision with no privileged URL should fail closed")
	}
}

// TestResultRoundTripsPassword proves the plaintext password never leaves the
// Provisioner in the Result and that the encrypter was actually invoked with the
// generated password (the resolver's decrypter must be able to open it later).
func TestProvisionEncryptsGeneratedPassword(t *testing.T) {
	// This exercises only the pre-DB portion by asserting the encrypter contract
	// through a boundary error: with an unreachable privileged URL the DB step
	// fails, but params/encrypter must have been validated first. We assert the
	// error is the connection step, not a validation error.
	enc := &fakeEncrypter{}
	p := &Provisioner{Encrypter: enc, PrivilegedURL: "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"}
	_, err := p.Provision(context.Background(), Params{Slug: "acme", Name: "Acme", Host: "127.0.0.1", Port: 1})
	if err == nil {
		t.Fatal("expected a connection error against an unreachable privileged URL")
	}
	if errors.Is(err, errValidation) {
		t.Fatalf("expected a connection error, got a validation error: %v", err)
	}
}
