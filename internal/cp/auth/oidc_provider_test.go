package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// stubIDP is a minimal in-process OpenID Connect provider: it serves a discovery
// document, a JWKS, and a token endpoint that returns a signed id_token. It is
// used by these provider unit tests and, in a build-tagged form, by the e2e
// suite — so the OIDC flow is proven against a real HTTP IdP without reaching any
// external network.
type stubIDP struct {
	server        *httptest.Server
	key           *rsa.PrivateKey
	keyID         string
	clientID      string
	subject       string
	email         string
	emailVerified bool
	name          string
	// captured from the last token request
	lastCode     string
	lastVerifier string
	// nonceOverride, when set, is put in the id_token instead of the request's
	// (used to prove nonce validation).
	nonceOverride string
	pendingNonce  string
}

func newStubIDP(t *testing.T, clientID string) *stubIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	idp := &stubIDP{
		key:           key,
		keyID:         "test-key",
		clientID:      clientID,
		subject:       "stub-subject",
		email:         "stub@example.test",
		emailVerified: true,
		name:          "Stub User",
	}
	mux := http.NewServeMux()
	idp.server = httptest.NewServer(mux)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                idp.server.URL,
			"authorization_endpoint":                idp.server.URL + "/authorize",
			"token_endpoint":                        idp.server.URL + "/token",
			"jwks_uri":                              idp.server.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       key.Public(),
			KeyID:     idp.keyID,
			Algorithm: "RS256",
			Use:       "sig",
		}}}
		writeJSON(w, jwks)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		idp.lastCode = r.Form.Get("code")
		idp.lastVerifier = r.Form.Get("code_verifier")
		nonce := idp.pendingNonce
		if idp.nonceOverride != "" {
			nonce = idp.nonceOverride
		}
		writeJSON(w, map[string]any{
			"access_token": "stub-access",
			"token_type":   "Bearer",
			"id_token":     idp.signIDToken(t, nonce),
		})
	})
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *stubIDP) signIDToken(t *testing.T, nonce string) string {
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
		"nonce":          nonce,
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestProviderVerifiesSignedIDToken proves NewOIDCProvider wires a real go-oidc
// verifier + oauth2 PKCE exchange against a live (stub) discovery document: a
// signed id_token with the right nonce verifies, and the exchange forwards the
// PKCE verifier.
func TestProviderVerifiesSignedIDToken(t *testing.T) {
	idp := newStubIDP(t, "test-client")
	cfg := OIDCConfig{Issuer: idp.server.URL, ClientID: "test-client", ClientSecret: "test-secret", RedirectURL: "https://app/callback"}
	prov, err := NewOIDCProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}

	// The auth URL must carry the S256 challenge for a known verifier.
	_, st, err := prov.AuthCodeURL(context.Background())
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	idp.pendingNonce = st.Nonce

	raw, err := prov.Exchanger.Exchange(context.Background(), "the-code", st.CodeVerifier)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if idp.lastVerifier != st.CodeVerifier {
		t.Fatalf("token endpoint got code_verifier %q, want %q", idp.lastVerifier, st.CodeVerifier)
	}
	claims, err := prov.Verifier.Verify(context.Background(), raw, st.Nonce)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Subject != "stub-subject" || claims.Email != "stub@example.test" || !claims.EmailVerified {
		t.Fatalf("claims = %+v", claims)
	}
}

// TestProviderRejectsBadNonce proves the go-oidc verifier rejects an id_token
// whose nonce differs from the expected one.
func TestProviderRejectsBadNonce(t *testing.T) {
	idp := newStubIDP(t, "test-client")
	idp.nonceOverride = "attacker-nonce"
	cfg := OIDCConfig{Issuer: idp.server.URL, ClientID: "test-client", ClientSecret: "test-secret", RedirectURL: "https://app/callback"}
	prov, err := NewOIDCProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	_, st, _ := prov.AuthCodeURL(context.Background())
	raw, err := prov.Exchanger.Exchange(context.Background(), "c", st.CodeVerifier)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if _, err := prov.Verifier.Verify(context.Background(), raw, st.Nonce); err == nil {
		t.Fatal("verifier accepted an id_token with the wrong nonce")
	}
}

// TestProviderAuthURLUsesDiscoveredEndpoint proves the built URL points at the
// discovered authorization_endpoint and carries the S256 challenge.
func TestProviderAuthURLUsesDiscoveredEndpoint(t *testing.T) {
	idp := newStubIDP(t, "test-client")
	cfg := OIDCConfig{Issuer: idp.server.URL, ClientID: "test-client", ClientSecret: "test-secret", RedirectURL: "https://app/callback"}
	prov, err := NewOIDCProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	rawURL, st, err := prov.AuthCodeURL(context.Background())
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.HasPrefix(rawURL, idp.server.URL+"/authorize") {
		t.Fatalf("auth url %q does not use discovered endpoint", rawURL)
	}
	wantChallenge := base64.RawURLEncoding.EncodeToString(sha256Sum(st.CodeVerifier))
	if got := u.Query().Get("code_challenge"); got != wantChallenge {
		t.Fatalf("code_challenge = %q, want S256(verifier) %q", got, wantChallenge)
	}
}

func sha256Sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
