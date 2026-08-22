package auth

import (
	"strings"
	"testing"
)

// TestParseScopeRejectsUnknown proves scopes are exactly the SPEC-07 §2 set
// (query, ingest, admin) and nothing else is accepted, so no invented scope
// enters the system (FR-ACC-04).
func TestParseScopeRejectsUnknown(t *testing.T) {
	for _, s := range []string{"query", "ingest", "admin"} {
		if _, err := ParseScope(s); err != nil {
			t.Fatalf("ParseScope(%q) = %v, want ok", s, err)
		}
	}
	for _, s := range []string{"", "platform", "write", "QUERY", "delete_tenant"} {
		if _, err := ParseScope(s); err == nil {
			t.Fatalf("ParseScope(%q) = ok, want error", s)
		}
	}
}

// TestScopeSetHas checks membership on a parsed scope set.
func TestScopeSetHas(t *testing.T) {
	set, err := ParseScopes([]string{"query", "ingest"})
	if err != nil {
		t.Fatalf("ParseScopes: %v", err)
	}
	if !set.Has(ScopeQuery) || !set.Has(ScopeIngest) {
		t.Fatal("set missing a granted scope")
	}
	if set.Has(ScopeAdmin) {
		t.Fatal("set has a scope it was not granted")
	}
}

// TestParseScopesRejectsEmpty ensures a key must carry at least one scope.
func TestParseScopesRejectsEmpty(t *testing.T) {
	if _, err := ParseScopes(nil); err == nil {
		t.Fatal("ParseScopes(nil) = ok, want error (a key needs a scope)")
	}
}

// TestNewAPIKeySecretFormat proves the minted secret matches the SPEC-02 §3
// wire format rk_<prefix>_<secret>: a stable, indexable prefix plus 32 bytes of
// base64url entropy, and that the stored prefix is exactly the lookup prefix.
func TestNewAPIKeySecretFormat(t *testing.T) {
	secret, prefix, hash, err := newAPIKeySecret()
	if err != nil {
		t.Fatalf("newAPIKeySecret: %v", err)
	}
	if !strings.HasPrefix(secret, "rk_") {
		t.Fatalf("secret %q missing rk_ scheme", secret)
	}
	// The secret body is base64url and may itself contain '_', so the wire
	// format is parsed as rk_<prefix>_<rest> (SplitN into 3), never a plain
	// Split. This is exactly how the verifier recovers the prefix for lookup.
	parts := strings.SplitN(secret, "_", 3)
	if len(parts) != 3 || parts[0] != "rk" {
		t.Fatalf("secret %q not rk_<prefix>_<secret>", secret)
	}
	if parts[1] != prefix {
		t.Fatalf("returned prefix %q != secret prefix %q", prefix, parts[1])
	}
	if len(prefix) != apiKeyPrefixLen {
		t.Fatalf("prefix len = %d, want %d", len(prefix), apiKeyPrefixLen)
	}
	// The hash must be the sha256 of the FULL presented secret, so verification
	// re-hashes the whole Bearer value.
	if want := hashToken(secret); string(hash) != string(want) {
		t.Fatal("stored hash is not sha256(full secret)")
	}
}

// TestAPIKeySecretsAreUnique guards against a broken RNG returning a constant.
func TestAPIKeySecretsAreUnique(t *testing.T) {
	a, _, _, err := newAPIKeySecret()
	if err != nil {
		t.Fatalf("newAPIKeySecret: %v", err)
	}
	b, _, _, err := newAPIKeySecret()
	if err != nil {
		t.Fatalf("newAPIKeySecret: %v", err)
	}
	if a == b {
		t.Fatal("two minted secrets are identical")
	}
}
