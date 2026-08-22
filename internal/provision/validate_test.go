package provision

import "testing"

// TestValidateAppliesDefaults proves validate fills in the schema-default
// embedding dimension, the default sslmode, and inherits the Provisioner's
// TenantHost/TenantPort when Params leaves them zero.
func TestValidateAppliesDefaults(t *testing.T) {
	p := &Provisioner{
		Encrypter:     &fakeEncrypter{},
		PrivilegedURL: "postgres://x",
		TenantHost:    "pg-1",
		TenantPort:    6432,
	}
	got, err := p.validate(Params{Slug: "acme", Name: "Acme"})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.EmbeddingDim != defaultEmbeddingDim {
		t.Errorf("EmbeddingDim = %d, want default %d", got.EmbeddingDim, defaultEmbeddingDim)
	}
	if got.SSLMode != "require" {
		t.Errorf("SSLMode = %q, want require", got.SSLMode)
	}
	if got.Host != "pg-1" || got.Port != 6432 {
		t.Errorf("host:port = %s:%d, want pg-1:6432 (inherited)", got.Host, got.Port)
	}
}

// TestValidateKeepsExplicitParams proves explicit Params values win over the
// Provisioner defaults.
func TestValidateKeepsExplicitParams(t *testing.T) {
	p := &Provisioner{Encrypter: &fakeEncrypter{}, PrivilegedURL: "postgres://x", TenantHost: "pg-1", TenantPort: 1}
	got, err := p.validate(Params{Slug: "acme", Name: "Acme", Host: "explicit", Port: 9999, SSLMode: "disable", EmbeddingDim: 768})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.Host != "explicit" || got.Port != 9999 || got.SSLMode != "disable" || got.EmbeddingDim != 768 {
		t.Errorf("explicit params not preserved: %+v", got)
	}
}

// TestAdminURLForDatabaseRewritesPath proves the privileged URL is repointed at
// the tenant database while keeping credentials, host and options.
func TestAdminURLForDatabaseRewritesPath(t *testing.T) {
	out, err := adminURLForDatabase("postgres://super:pw@dbhost:5432/postgres?sslmode=disable", "tenant_acme_ab12cd34")
	if err != nil {
		t.Fatalf("adminURLForDatabase: %v", err)
	}
	const want = "postgres://super:pw@dbhost:5432/tenant_acme_ab12cd34?sslmode=disable"
	if out != want {
		t.Errorf("adminURLForDatabase = %q, want %q", out, want)
	}
}
