package tenant

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestDSNBuildsConnectionString(t *testing.T) {
	rec := Record{
		Host: "db.example.com", Port: 6432,
		Database: "tenant_acme", Username: "role_acme", SSLMode: "verify-full",
	}
	got := dsn(rec, "s3cr3t")

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("dsn produced unparseable URL %q: %v", got, err)
	}
	if u.Scheme != "postgres" {
		t.Fatalf("scheme = %q, want postgres", u.Scheme)
	}
	if u.Host != "db.example.com:6432" {
		t.Fatalf("host = %q, want db.example.com:6432", u.Host)
	}
	if u.Path != "/tenant_acme" {
		t.Fatalf("path = %q, want /tenant_acme", u.Path)
	}
	pw, _ := u.User.Password()
	if u.User.Username() != "role_acme" || pw != "s3cr3t" {
		t.Fatalf("user info = %q, want role_acme/s3cr3t", u.User.String())
	}
	if u.Query().Get("sslmode") != "verify-full" {
		t.Fatalf("sslmode = %q, want verify-full", u.Query().Get("sslmode"))
	}
}

func TestDSNEscapesSpecialCharsInPassword(t *testing.T) {
	rec := Record{Host: "h", Port: 5432, Database: "d", Username: "u"}
	got := dsn(rec, "p@ss/w:rd?x")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("unparseable: %v", err)
	}
	if pw, _ := u.User.Password(); pw != "p@ss/w:rd?x" {
		t.Fatalf("password round-trip = %q, want p@ss/w:rd?x", pw)
	}
}

func TestDSNDefaultsSSLModeToRequire(t *testing.T) {
	rec := Record{Host: "h", Port: 5432, Database: "d", Username: "u"} // SSLMode empty
	got := dsn(rec, "x")
	if !strings.Contains(got, "sslmode=require") {
		t.Fatalf("dsn %q missing default sslmode=require", got)
	}
}

// errDecrypter always fails, standing in for a wrong-key / tampered ciphertext.
type errDecrypter struct{ err error }

func (e errDecrypter) Decrypt([]byte) ([]byte, error) { return nil, e.err }

func TestPasswordBuilderRejectsNilDecrypter(t *testing.T) {
	build := passwordDecryptingBuilder(nil)
	if _, err := build(context.Background(), Record{ID: ID(uuid.New())}); err == nil {
		t.Fatal("build with nil decrypter: want error, got nil")
	}
}

func TestPasswordBuilderReturnsDecryptError(t *testing.T) {
	sentinel := errors.New("boom")
	build := passwordDecryptingBuilder(errDecrypter{err: sentinel})
	_, err := build(context.Background(), Record{ID: ID(uuid.New()), PasswordEnc: []byte("x")})
	if !errors.Is(err, sentinel) {
		t.Fatalf("build: want wrapped decrypt error, got %v", err)
	}
}
