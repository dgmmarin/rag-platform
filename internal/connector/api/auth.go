package api

import (
	"context"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/rag-platform/ragctl/internal/connector"
)

// authClient is an HTTP client with the source's authentication applied. For
// header-based auth (api_key_header/bearer/basic) it wraps the base SSRF-guarded
// client with a per-request decorator that sets the auth header. For oauth2_cc the
// client is an x/oauth2 token-refreshing client whose transport is composed OVER the
// guarded transport, so both the token-endpoint fetch and every API call dial through
// the SSRF guard; the decorator is then nil (the token is applied by the transport).
type authClient struct {
	client   *http.Client
	decorate func(*http.Request)
}

func (a authClient) do(req *http.Request) (*http.Response, error) {
	if a.decorate != nil {
		a.decorate(req)
	}
	return a.client.Do(req)
}

// buildAuthedClient composes base (the SSRF-guarded egress client) with the source's
// auth and decrypted credentials (SPEC-04 §4/§6). It fails closed if a required
// secret is missing. Errors never contain secret values (only which key is missing),
// so a Test/Sync failure is safe to surface.
func buildAuthedClient(ctx context.Context, base *http.Client, auth authConfig, creds connector.Credentials) (authClient, error) {
	switch auth.Type {
	case "api_key_header":
		key, ok := creds[credKeyAPIKey]
		if !ok || key == "" {
			return authClient{}, fmt.Errorf("api: auth %q requires credential %q", auth.Type, credKeyAPIKey)
		}
		header := auth.Header
		if header == "" {
			header = "X-API-Key"
		}
		return authClient{client: base, decorate: func(r *http.Request) { r.Header.Set(header, key) }}, nil

	case "bearer":
		tok, ok := creds[credKeyToken]
		if !ok || tok == "" {
			return authClient{}, fmt.Errorf("api: auth %q requires credential %q", auth.Type, credKeyToken)
		}
		return authClient{client: base, decorate: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }}, nil

	case "basic":
		user, uok := creds[credKeyUsername]
		pass, pok := creds[credKeyPassword]
		if !uok || !pok || user == "" {
			return authClient{}, fmt.Errorf("api: auth %q requires credentials %q and %q", auth.Type, credKeyUsername, credKeyPassword)
		}
		return authClient{client: base, decorate: func(r *http.Request) { r.SetBasicAuth(user, pass) }}, nil

	case "oauth2_cc":
		id, iok := creds[credKeyClientID]
		secret, sok := creds[credKeyClientSecret]
		if !iok || !sok || id == "" || secret == "" {
			return authClient{}, fmt.Errorf("api: auth %q requires credentials %q and %q", auth.Type, credKeyClientID, credKeyClientSecret)
		}
		if auth.TokenURL == "" {
			return authClient{}, fmt.Errorf("api: auth %q requires token_url", auth.Type)
		}
		// Thread the SSRF-guarded base client into the oauth2 context. x/oauth2 uses
		// this client for BOTH the token-endpoint request and (as the oauth2.Transport
		// Base) every API call, so the token endpoint is SSRF-guarded too and we do NOT
		// hand-roll token fetch/refresh/caching — clientcredentials.TokenSource does it.
		guardedCtx := context.WithValue(ctx, oauth2.HTTPClient, base)
		cc := &clientcredentials.Config{
			ClientID:     id,
			ClientSecret: secret,
			TokenURL:     auth.TokenURL,
			Scopes:       auth.Scopes,
		}
		return authClient{client: cc.Client(guardedCtx)}, nil

	default:
		// Unreachable: ValidateConfig's enum rejects any other value before Sync.
		return authClient{}, fmt.Errorf("api: unsupported auth type %q", auth.Type)
	}
}
