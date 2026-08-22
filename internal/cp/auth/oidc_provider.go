package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// oidc_provider.go is the only file that touches go-oidc/oauth2. It turns an
// OIDCConfig into a live provider (discovery, JWKS-backed verifier, PKCE
// authorization-code exchange) and produces the Exchanger/Verifier the
// OIDCService depends on. Keeping the concrete library here means the flow logic
// in oidc.go stays unit-testable with stubs (NFR-MNT-01: swapping the IdP needs
// no change outside this file).

// oidcProvider adapts go-oidc + oauth2 to the Exchanger and Verifier interfaces.
type oidcProvider struct {
	cfg      OIDCConfig
	oauth2   oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewOIDCProvider performs OIDC discovery against the configured issuer and
// returns an OIDCService whose Exchanger/Verifier are backed by the real
// library. The context bounds the discovery HTTP call only.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (*OIDCService, error) {
	if !cfg.Enabled() {
		return nil, errors.New("auth: oidc not configured")
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		// The wrapped error may reference the issuer URL but never a secret.
		return nil, fmt.Errorf("auth: oidc discovery: %w", err)
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}
	p := &oidcProvider{
		cfg: cfg,
		oauth2: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       scopes,
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}

	return &OIDCService{
		Config:    cfg,
		Exchanger: p,
		Verifier:  p,
		provider:  p,
	}, nil
}

// buildAuthURL constructs the authorization request against the discovered
// endpoint, adding the PKCE S256 challenge and the nonce (oauth2 handles state).
func (p *oidcProvider) buildAuthURL(st LoginState) string {
	return p.oauth2.AuthCodeURL(st.State,
		oidc.Nonce(st.Nonce),
		oauth2.S256ChallengeOption(st.CodeVerifier),
	)
}

// Exchange trades the authorization code (with the PKCE verifier) for tokens and
// returns the raw id_token. A response missing id_token is an error, not a
// silent success. No token value is ever logged.
func (p *oidcProvider) Exchange(ctx context.Context, code, codeVerifier string) (string, error) {
	tok, err := p.oauth2.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return "", fmt.Errorf("auth: oauth2 exchange: %w", err)
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return "", errors.New("auth: token response missing id_token")
	}
	return rawIDToken, nil
}

// Verify validates the id_token (signature via JWKS, issuer, audience, expiry)
// and the nonce, returning the subset of claims the login flow needs.
func (p *oidcProvider) Verify(ctx context.Context, rawIDToken, wantNonce string) (Claims, error) {
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Claims{}, fmt.Errorf("auth: verify id_token: %w", err)
	}
	if idToken.Nonce != wantNonce {
		return Claims{}, ErrOIDCNonceMismatch
	}
	var c struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&c); err != nil {
		return Claims{}, fmt.Errorf("auth: decode id_token claims: %w", err)
	}
	return Claims{
		Subject:       idToken.Subject,
		Email:         c.Email,
		EmailVerified: c.EmailVerified,
		Name:          c.Name,
		Nonce:         idToken.Nonce,
	}, nil
}
