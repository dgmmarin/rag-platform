package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/egress"
)

// TestOAuth2TokenEndpointSSRFGuarded proves the oauth2 token-endpoint fetch dials
// through the SSRF guard: with the REAL guarded client as the base, a token_url that
// resolves to loopback is blocked (egress.ErrBlocked) before any token is issued. If
// the guarded transport were not composed under the oauth2 client, this would
// silently reach the internal address.
func TestOAuth2TokenEndpointSSRFGuarded(t *testing.T) {
	srv, _, _ := newAuthFixture(t, "oauth2_cc", "none") // a loopback (127.0.0.1) server
	guarded := egress.GuardedClient(fetchTimeout, maxRedirects)

	ac, err := buildAuthedClient(context.Background(), guarded,
		authConfigFor("oauth2_cc", srv.URL+"/token"), connector.Credentials(credsFor("oauth2_cc")))
	if err != nil {
		t.Fatalf("buildAuthedClient: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/items", nil)
	resp, err := ac.do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected SSRF block fetching token from loopback, got 200")
	}
	if !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("error = %v; want egress.ErrBlocked (token endpoint not SSRF-guarded)", err)
	}
}

// TestAuthAppliedPerType proves each of the four auth types authenticates a request
// against a fixture that rejects (401) anything missing the right secret. Secrets
// come from connector.Credentials, the auth *shape* from config.
func TestAuthAppliedPerType(t *testing.T) {
	for _, at := range authTypes {
		at := at
		t.Run(at, func(t *testing.T) {
			srv, _, _ := newAuthFixture(t, at, "none")

			auth := authConfigFor(at, srv.URL+"/token")
			ac, err := buildAuthedClient(context.Background(), srv.Client(), auth, connector.Credentials(credsFor(at)))
			if err != nil {
				t.Fatalf("buildAuthedClient(%s): %v", at, err)
			}

			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/items", nil)
			resp, err := ac.do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("auth %s: status %d, want 200 (secret not applied)", at, resp.StatusCode)
			}
		})
	}
}

// TestAuthRejectsWrongSecret confirms the fixture actually enforces auth (so the
// green above is meaningful, not a fixture that lets everything through).
func TestAuthRejectsWrongSecret(t *testing.T) {
	srv, _, _ := newAuthFixture(t, "bearer", "none")
	auth := authConfigFor("bearer", "")
	ac, err := buildAuthedClient(context.Background(), srv.Client(), auth, connector.Credentials{credKeyToken: "wrong"})
	if err != nil {
		t.Fatalf("buildAuthedClient: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/items", nil)
	resp, err := ac.do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong bearer token: status %d, want 401", resp.StatusCode)
	}
}

// TestAuthMissingCredential fails closed when a required secret is absent.
func TestAuthMissingCredential(t *testing.T) {
	if _, err := buildAuthedClient(context.Background(), http.DefaultClient, authConfig{Type: "bearer"}, connector.Credentials{}); err == nil {
		t.Fatalf("bearer with no token should error")
	}
	if _, err := buildAuthedClient(context.Background(), http.DefaultClient, authConfig{Type: "oauth2_cc", TokenURL: "https://x/token"}, connector.Credentials{credKeyClientID: "only-id"}); err == nil {
		t.Fatalf("oauth2_cc missing client_secret should error")
	}
}
