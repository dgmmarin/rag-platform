package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// OIDC login (STORY-03.2, FR-ACC-01, SPEC-02 §3, SPEC-09 §3). The authorization-
// code + PKCE flow, state/nonce validation, and just-in-time provisioning /
// link-by-verified-email are implemented here as a service so the branch logic is
// unit-testable with a stubbed IdP; the concrete go-oidc/oauth2 wiring lives in
// oidc_provider.go. Mounting the two HTTP handlers on the public router is
// STORY-04.1 (mirrors the password-auth deferral). No client secret or token is
// ever logged (C-4, SPEC-09 §2).

// OIDC sentinel errors. Callback collapses provider/verifier failures into these
// so HTTP handlers (STORY-04.1) can map them to responses without string
// matching. ErrOIDCStateMismatch and ErrOIDCNonceMismatch guard the CSRF/replay
// properties; ErrOIDCEmailUnverified enforces the verified-email link rule;
// ErrOIDCUserNotProvisioned is returned when JIT is disabled and no matching
// user exists.
var (
	ErrOIDCStateMismatch      = errors.New("auth: oidc state mismatch")
	ErrOIDCNonceMismatch      = errors.New("auth: oidc nonce mismatch")
	ErrOIDCEmailUnverified    = errors.New("auth: oidc email not verified")
	ErrOIDCUserNotProvisioned = errors.New("auth: oidc user not provisioned")
	ErrOIDCNoEmail            = errors.New("auth: oidc token has no email claim")
	ErrOIDCNoSubject          = errors.New("auth: oidc token has no subject claim")
)

// OIDCConfig is the per-deployment provider configuration (loaded from
// internal/config). ClientSecret is confidential and never logged.
type OIDCConfig struct {
	Issuer          string
	ClientID        string
	ClientSecret    string
	RedirectURL     string
	Scopes          []string // defaults to openid,email,profile when empty
	JITProvisioning bool     // create the user on first login when true
}

// Enabled reports whether OIDC is configured for this deployment.
func (c OIDCConfig) Enabled() bool {
	return c.Issuer != "" && c.ClientID != "" && c.RedirectURL != ""
}

// Claims is the subset of verified id_token claims the login flow needs. It is
// produced by a Verifier from a raw id_token that has already passed signature,
// issuer, audience, and expiry checks.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Nonce         string
}

// LoginState is the per-request material minted by AuthCodeURL that must be
// carried across the redirect and presented at Callback. In STORY-04.1 it is
// stored in a short-lived, HttpOnly, signed cookie; unit tests pass it directly.
// It is opaque to callers and holds only single-use secrets.
type LoginState struct {
	State        string // CSRF token echoed as the `state` query param
	Nonce        string // replay guard bound into the id_token
	CodeVerifier string // PKCE verifier; its S256 hash is the code_challenge
}

// CallbackParams are the query parameters the IdP redirects back with.
type CallbackParams struct {
	Code  string
	State string
}

// Exchanger performs the authorization-code -> token exchange with PKCE and
// returns the raw id_token. Implemented by the oauth2-backed provider; stubbed
// in unit tests.
type Exchanger interface {
	Exchange(ctx context.Context, code, codeVerifier string) (rawIDToken string, err error)
}

// Verifier validates a raw id_token (signature/issuer/audience/expiry) against
// the expected nonce and returns its claims. Implemented by the go-oidc verifier;
// stubbed in unit tests.
type Verifier interface {
	Verify(ctx context.Context, rawIDToken, wantNonce string) (Claims, error)
}

// OIDCService drives the OIDC login flow and mints a control-plane session
// (reusing Service, the same session store as password login).
type OIDCService struct {
	Auth      *Service
	Config    OIDCConfig
	Exchanger Exchanger
	Verifier  Verifier
	Now       Clock

	// provider, when set (by NewOIDCProvider), builds the authorization URL
	// against the discovered endpoint. Focused unit tests leave it nil and fall
	// back to the standard-parameter builder.
	provider authURLBuilder
}

// authURLBuilder builds the provider authorization URL for a LoginState. The
// oauth2-backed provider implements it; unit tests exercising only the flow
// logic leave OIDCService.provider nil.
type authURLBuilder interface {
	buildAuthURL(st LoginState) string
}

func (o *OIDCService) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// pkceVerifierBytes / stateBytes / nonceBytes are 32 bytes = 256 bits of entropy
// each (RFC 7636 recommends 43-128 chars of verifier; 32 raw bytes base64url is
// 43 chars).
const (
	pkceVerifierBytes = 32
	stateBytes        = 32
	nonceBytes        = 32
)

// AuthCodeURL mints fresh state, nonce, and a PKCE verifier, and returns the
// provider authorization URL (carrying state, nonce, and the S256 code
// challenge) together with the LoginState the caller must persist for Callback.
func (o *OIDCService) AuthCodeURL(ctx context.Context) (string, LoginState, error) {
	if !o.Config.Enabled() {
		return "", LoginState{}, errors.New("auth: oidc not configured")
	}
	state, err := randomToken(stateBytes)
	if err != nil {
		return "", LoginState{}, err
	}
	nonce, err := randomToken(nonceBytes)
	if err != nil {
		return "", LoginState{}, err
	}
	verifier, err := randomToken(pkceVerifierBytes)
	if err != nil {
		return "", LoginState{}, err
	}
	st := LoginState{State: state, Nonce: nonce, CodeVerifier: verifier}

	authURL, err := o.buildAuthCodeURL(ctx, st)
	if err != nil {
		return "", LoginState{}, err
	}
	return authURL, st, nil
}

// pkceChallenge returns the RFC 7636 S256 code challenge for a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Callback validates state, exchanges the code (with the PKCE verifier),
// verifies the id_token against the expected nonce, enforces the verified-email
// rule, links or JIT-creates the user, and mints a session. The returned Session
// carries the plaintext cookie token exactly once (as for password login).
func (o *OIDCService) Callback(ctx context.Context, p CallbackParams, st LoginState) (Session, error) {
	// State is the CSRF guard: reject before any token exchange (constant-time so
	// a mismatch is not a timing oracle).
	if st.State == "" || subtle.ConstantTimeCompare([]byte(p.State), []byte(st.State)) != 1 {
		return Session{}, ErrOIDCStateMismatch
	}

	rawIDToken, err := o.Exchanger.Exchange(ctx, p.Code, st.CodeVerifier)
	if err != nil {
		return Session{}, fmt.Errorf("auth: oidc code exchange: %w", err)
	}

	claims, err := o.Verifier.Verify(ctx, rawIDToken, st.Nonce)
	if err != nil {
		return Session{}, err
	}

	if claims.Subject == "" {
		return Session{}, ErrOIDCNoSubject
	}
	if claims.Email == "" {
		return Session{}, ErrOIDCNoEmail
	}
	// Link/JIT only ever happen on a provider-verified email (SPEC-09 §3, the AC
	// "existing user linked by verified email"): an unverified email must never
	// take over an existing account.
	if !claims.EmailVerified {
		return Session{}, ErrOIDCEmailUnverified
	}

	userID, err := o.resolveUser(ctx, claims)
	if err != nil {
		return Session{}, err
	}

	now := o.now()
	if _, err := o.Auth.DB.Exec(ctx,
		`update users set last_login_at = $2 where id = $1`, userID, now); err != nil {
		return Session{}, fmt.Errorf("auth: oidc stamp last_login: %w", err)
	}
	return o.Auth.createSession(ctx, userID, now)
}

// resolveUser maps verified claims to a user id, in priority order:
//  1. an existing identity link for (issuer, subject) -> that user;
//  2. an existing user with the matching (verified) email -> link this identity
//     to it (account linking);
//  3. no match: JIT-create the user and its identity when provisioning is
//     enabled, otherwise refuse (ErrOIDCUserNotProvisioned).
func (o *OIDCService) resolveUser(ctx context.Context, claims Claims) (string, error) {
	issuer := o.Config.Issuer

	// (1) Existing identity link.
	var userID string
	err := o.Auth.DB.QueryRow(ctx,
		`select user_id::text from user_identities where issuer = $1 and subject = $2`,
		issuer, claims.Subject).Scan(&userID)
	switch {
	case err == nil:
		return userID, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to email match / JIT
	default:
		return "", fmt.Errorf("auth: oidc look up identity: %w", err)
	}

	email := normalizeEmail(claims.Email)

	// (2) Existing user by verified email -> link.
	err = o.Auth.DB.QueryRow(ctx,
		`select id::text from users where email = $1`, email).Scan(&userID)
	switch {
	case err == nil:
		if err := o.linkIdentity(ctx, userID, issuer, claims.Subject); err != nil {
			return "", err
		}
		return userID, nil
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to JIT
	default:
		return "", fmt.Errorf("auth: oidc look up user by email: %w", err)
	}

	// (3) JIT create, or refuse.
	if !o.Config.JITProvisioning {
		return "", ErrOIDCUserNotProvisioned
	}
	return o.jitCreate(ctx, claims, issuer)
}

// linkIdentity records the (issuer, subject) -> user link. A concurrent
// duplicate insert (unique violation) is treated as success.
func (o *OIDCService) linkIdentity(ctx context.Context, userID, issuer, subject string) error {
	if _, err := o.Auth.DB.Exec(ctx,
		`insert into user_identities (user_id, issuer, subject) values ($1, $2, $3)
		 on conflict (issuer, subject) do nothing`,
		userID, issuer, subject); err != nil {
		return fmt.Errorf("auth: oidc link identity: %w", err)
	}
	return nil
}

// jitCreate provisions a new password-less user (password_hash stays null so the
// account can only be reached via OIDC) plus its identity link.
func (o *OIDCService) jitCreate(ctx context.Context, claims Claims, issuer string) (string, error) {
	email := normalizeEmail(claims.Email)
	var displayName *string
	if n := strings.TrimSpace(claims.Name); n != "" {
		displayName = &n
	}

	var userID string
	err := o.Auth.DB.QueryRow(ctx,
		`insert into users (email, display_name, external_id) values ($1, $2, $3) returning id::text`,
		email, displayName, claims.Subject).Scan(&userID)
	if err != nil {
		if isUniqueViolation(err) {
			// Raced with another login for the same email: fall back to the existing
			// row and link to it.
			if scanErr := o.Auth.DB.QueryRow(ctx,
				`select id::text from users where email = $1`, email).Scan(&userID); scanErr != nil {
				return "", fmt.Errorf("auth: oidc jit resolve raced user: %w", scanErr)
			}
		} else {
			return "", fmt.Errorf("auth: oidc jit create user: %w", err)
		}
	}
	if err := o.linkIdentity(ctx, userID, issuer, claims.Subject); err != nil {
		return "", err
	}
	return userID, nil
}

// buildAuthCodeURL is overridden by the provider wiring (oidc_provider.go). The
// default (used when no oauth2 config is attached, e.g. focused unit tests)
// constructs the standard authorization request from Config so the PKCE/state/
// nonce parameters can be asserted without a live discovery document.
func (o *OIDCService) buildAuthCodeURL(_ context.Context, st LoginState) (string, error) {
	if o.provider != nil {
		return o.provider.buildAuthURL(st), nil
	}
	scopes := o.Config.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	q := url.Values{}
	q.Set("client_id", o.Config.ClientID)
	q.Set("redirect_uri", o.Config.RedirectURL)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", st.State)
	q.Set("nonce", st.Nonce)
	q.Set("code_challenge", pkceChallenge(st.CodeVerifier))
	q.Set("code_challenge_method", "S256")
	base := strings.TrimRight(o.Config.Issuer, "/") + "/authorize"
	return base + "?" + q.Encode(), nil
}
