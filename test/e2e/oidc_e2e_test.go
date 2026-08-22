//go:build e2e

// STORY-03.2 golden path: OIDC login against the REAL control-plane Postgres
// (up via `mise run up`), with an in-process stub IdP standing in for the token
// and discovery endpoints (no external IdP is contacted). It drives the real
// auth.OIDCService (NewOIDCProvider -> real go-oidc verifier + oauth2 PKCE
// exchange) through the two required flows:
//   - JIT creation on first login (JIT enabled): a brand-new verified email
//     creates the user and its (issuer, subject) identity link;
//   - link-by-verified-email on a subsequent login: a pre-existing user with the
//     same verified email gets the OIDC identity linked, and no duplicate user is
//     created;
//
// and asserts that state and nonce/PKCE mismatches are rejected without minting a
// session.
package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/rag-platform/ragctl/internal/cp/auth"
)

// e2eStubIDP is a minimal, real HTTP OpenID Connect provider used only by this
// e2e test: discovery + JWKS + a token endpoint returning an RS256-signed
// id_token. The subject/email/verified/nonce it asserts are configurable so a
// single provider can drive several login scenarios.
type e2eStubIDP struct {
	server        *httptest.Server
	key           *rsa.PrivateKey
	keyID         string
	clientID      string
	subject       string
	email         string
	emailVerified bool
	name          string
	pendingNonce  string
}

func newE2EStubIDP(t *testing.T, clientID string) *e2eStubIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	idp := &e2eStubIDP{key: key, keyID: "e2e-key", clientID: clientID, emailVerified: true, name: "OIDC User"}
	mux := http.NewServeMux()
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONE2E(w, map[string]any{
			"issuer":                                idp.server.URL,
			"authorization_endpoint":                idp.server.URL + "/authorize",
			"token_endpoint":                        idp.server.URL + "/token",
			"jwks_uri":                              idp.server.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONE2E(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: idp.keyID, Algorithm: "RS256", Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		writeJSONE2E(w, map[string]any{
			"access_token": "e2e-access",
			"token_type":   "Bearer",
			"id_token":     idp.signIDToken(t),
		})
	})
	return idp
}

func (idp *e2eStubIDP) signIDToken(t *testing.T) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: idp.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", idp.keyID))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	now := time.Now()
	claims := map[string]any{
		"iss":            idp.server.URL,
		"sub":            idp.subject,
		"aud":            idp.clientID,
		"exp":            now.Add(time.Hour).Unix(),
		"iat":            now.Unix(),
		"nonce":          idp.pendingNonce,
		"email":          idp.email,
		"email_verified": idp.emailVerified,
		"name":           idp.name,
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("sign id_token: %v", err)
	}
	return raw
}

func writeJSONE2E(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// runOIDCLogin performs one full AuthCodeURL -> (idp asserts nonce) -> Callback
// against the given service, returning the minted session.
func runOIDCLogin(ctx context.Context, t *testing.T, svc *auth.OIDCService, idp *e2eStubIDP) auth.Session {
	t.Helper()
	_, st, err := svc.AuthCodeURL(ctx)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	// The IdP echoes the request nonce into the id_token (a compliant provider
	// does exactly this); the code value is irrelevant to the stub token endpoint.
	idp.pendingNonce = st.Nonce
	sess, err := svc.Callback(ctx, auth.CallbackParams{Code: "auth-code", State: st.State}, st)
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}
	return sess
}

func TestOIDCLoginJITThenLinkGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	email := "oidc-" + mustSuffix(t) + "@example.test"
	subject := "sub-" + mustSuffix(t)

	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM users WHERE email = '%s'", email))
	})

	idp := newE2EStubIDP(t, "rag-admin")
	idp.subject = subject
	idp.email = email
	idp.emailVerified = true

	// --- Flow 1: first login with JIT enabled -> user + identity created. ---
	provJIT, err := auth.NewOIDCProvider(ctx, auth.OIDCConfig{
		Issuer:          idp.server.URL,
		ClientID:        "rag-admin",
		ClientSecret:    "e2e-secret",
		RedirectURL:     "https://app.example/callback",
		JITProvisioning: true,
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider (jit): %v", err)
	}
	provJIT.Auth = auth.NewService(auth.FromPool(pool))

	sess1 := runOIDCLogin(ctx, t, provJIT, idp)
	if sess1.Token == "" || sess1.CSRFToken == "" {
		t.Fatal("first OIDC login returned empty session token/csrf")
	}

	// A user row now exists for the verified email, with exactly one identity link
	// for (issuer, subject).
	if n := psqlScalar(t, fmt.Sprintf("select count(*) from users where email = '%s'", email)); n != "1" {
		t.Fatalf("user count after JIT = %q, want 1", n)
	}
	userID := psqlScalar(t, fmt.Sprintf("select id from users where email = '%s'", email))
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from user_identities where issuer = '%s' and subject = '%s' and user_id = '%s'",
		idp.server.URL, subject, userID)); n != "1" {
		t.Fatalf("identity link count after JIT = %q, want 1", n)
	}
	// The JIT user is password-less (OIDC-only): password_hash stays null.
	if h := psqlScalar(t, fmt.Sprintf("select coalesce(password_hash,'<null>') from users where email = '%s'", email)); h != "<null>" {
		t.Fatalf("JIT user has a password_hash %q; want null (OIDC-only)", h)
	}
	// The minted session is real: it resolves via the same session store.
	if _, err := provJIT.Auth.Lookup(ctx, sess1.Token); err != nil {
		t.Fatalf("session from OIDC login does not resolve: %v", err)
	}

	// --- Flow 2: a DIFFERENT issuer subject, same verified email, links to the
	// existing user (account linking) rather than creating a duplicate. ---
	idp2 := newE2EStubIDP(t, "rag-admin")
	subject2 := "sub2-" + mustSuffix(t)
	idp2.subject = subject2
	idp2.email = email
	idp2.emailVerified = true

	prov2, err := auth.NewOIDCProvider(ctx, auth.OIDCConfig{
		Issuer:          idp2.server.URL,
		ClientID:        "rag-admin",
		ClientSecret:    "e2e-secret",
		RedirectURL:     "https://app.example/callback",
		JITProvisioning: false, // even with JIT OFF, an existing verified email links
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider (link): %v", err)
	}
	prov2.Auth = auth.NewService(auth.FromPool(pool))

	sess2 := runOIDCLogin(ctx, t, prov2, idp2)
	if sess2.UserID != userID {
		t.Fatalf("link login resolved user %q, want existing %q", sess2.UserID, userID)
	}
	// Still exactly one user for the email (no duplicate), now with two identities.
	if n := psqlScalar(t, fmt.Sprintf("select count(*) from users where email = '%s'", email)); n != "1" {
		t.Fatalf("user count after link = %q, want 1 (no duplicate)", n)
	}
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from user_identities where user_id = '%s'", userID)); n != "2" {
		t.Fatalf("identity count for user after link = %q, want 2", n)
	}
}

func TestOIDCCallbackRejectsStateAndNonceMismatch(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	email := "oidc-mismatch-" + mustSuffix(t) + "@example.test"
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM users WHERE email = '%s'", email))
	})

	idp := newE2EStubIDP(t, "rag-admin")
	idp.subject = "sub-mismatch"
	idp.email = email

	prov, err := auth.NewOIDCProvider(ctx, auth.OIDCConfig{
		Issuer:          idp.server.URL,
		ClientID:        "rag-admin",
		ClientSecret:    "e2e-secret",
		RedirectURL:     "https://app.example/callback",
		JITProvisioning: true,
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	prov.Auth = auth.NewService(auth.FromPool(pool))

	// State mismatch: the returned state differs from the stored one -> rejected
	// before any token exchange, no user created.
	_, st, err := prov.AuthCodeURL(ctx)
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	idp.pendingNonce = st.Nonce
	if _, err := prov.Callback(ctx, auth.CallbackParams{Code: "c", State: "tampered-state"}, st); err == nil {
		t.Fatal("callback accepted a mismatched state")
	}

	// Nonce mismatch: the id_token carries a nonce the request never asked for ->
	// the go-oidc verifier check fails.
	_, st2, err := prov.AuthCodeURL(ctx)
	if err != nil {
		t.Fatalf("AuthCodeURL (2): %v", err)
	}
	idp.pendingNonce = "attacker-controlled-nonce" // != st2.Nonce
	if _, err := prov.Callback(ctx, auth.CallbackParams{Code: "c", State: st2.State}, st2); err == nil {
		t.Fatal("callback accepted an id_token with a mismatched nonce")
	}

	// Neither rejected attempt created a user.
	if n := psqlScalar(t, fmt.Sprintf("select count(*) from users where email = '%s'", email)); n != "0" {
		t.Fatalf("user created despite rejected logins: count = %q", n)
	}
}
