package auth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// fakeIDToken is a stub of the verified id_token claims the verifier yields, so
// the JIT/link/session branch logic can be unit-tested without a real IdP (the
// discovery+token endpoints are exercised end to end in the e2e suite).
type fakeVerifier struct {
	claims Claims
	err    error
}

func (f fakeVerifier) Verify(_ context.Context, _, wantNonce string) (Claims, error) {
	if f.err != nil {
		return Claims{}, f.err
	}
	if f.claims.Nonce != "" && f.claims.Nonce != wantNonce {
		return Claims{}, ErrOIDCNonceMismatch
	}
	return f.claims, nil
}

// fakeExchanger stubs the authorization-code -> token exchange.
type fakeExchanger struct {
	rawIDToken string
	err        error
	gotCode    string
	gotVerif   string
}

func (f *fakeExchanger) Exchange(_ context.Context, code, verifier string) (string, error) {
	f.gotCode = code
	f.gotVerif = verifier
	if f.err != nil {
		return "", f.err
	}
	return f.rawIDToken, nil
}

func newTestOIDC(t *testing.T, db DB, jit bool, ex Exchanger, ve Verifier) *OIDCService {
	t.Helper()
	return &OIDCService{
		Auth:      &Service{DB: db, Lockout: DefaultLockoutPolicy(), Now: func() time.Time { return time.Unix(1700000000, 0).UTC() }},
		Config:    OIDCConfig{ClientID: "client", RedirectURL: "https://app.example/callback", Issuer: "https://idp.example", JITProvisioning: jit},
		Exchanger: ex,
		Verifier:  ve,
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

// --- AuthCodeURL: builds a PKCE + state + nonce authorization request. ---

func TestAuthCodeURLIncludesPKCEStateNonce(t *testing.T) {
	svc := newTestOIDC(t, nil, true, &fakeExchanger{}, fakeVerifier{})
	url1, st, err := svc.AuthCodeURL(context.Background())
	if err != nil {
		t.Fatalf("AuthCodeURL: %v", err)
	}
	if st.State == "" || st.Nonce == "" || st.CodeVerifier == "" {
		t.Fatalf("LoginState missing fields: %+v", st)
	}
	assertQueryParam(t, url1, "state", st.State)
	assertQueryParam(t, url1, "nonce", st.Nonce)
	assertQueryParam(t, url1, "code_challenge_method", "S256")
	assertQueryParam(t, url1, "response_type", "code")
	if got := queryParam(t, url1, "code_challenge"); got == "" || got == st.CodeVerifier {
		t.Fatalf("code_challenge must be the S256 hash of the verifier, not empty or the raw verifier (got %q)", got)
	}

	// A second call yields fresh, unpredictable state/nonce/verifier.
	_, st2, err := svc.AuthCodeURL(context.Background())
	if err != nil {
		t.Fatalf("AuthCodeURL (2): %v", err)
	}
	if st.State == st2.State || st.Nonce == st2.Nonce || st.CodeVerifier == st2.CodeVerifier {
		t.Fatal("AuthCodeURL reused state/nonce/verifier across calls")
	}
}

// --- Callback: state mismatch is rejected before any token exchange. ---

func TestCallbackRejectsStateMismatch(t *testing.T) {
	ex := &fakeExchanger{rawIDToken: "irrelevant"}
	svc := newTestOIDC(t, nil, true, ex, fakeVerifier{})
	st := LoginState{State: "expected", Nonce: "n", CodeVerifier: "v"}

	_, err := svc.Callback(context.Background(), CallbackParams{Code: "c", State: "attacker"}, st)
	if !errors.Is(err, ErrOIDCStateMismatch) {
		t.Fatalf("state mismatch error = %v, want ErrOIDCStateMismatch", err)
	}
	if ex.gotCode != "" {
		t.Fatal("token exchange ran despite state mismatch (CSRF gate bypassed)")
	}
}

// --- Callback: nonce mismatch (from the verifier) is surfaced. ---

func TestCallbackRejectsNonceMismatch(t *testing.T) {
	ex := &fakeExchanger{rawIDToken: "tok"}
	ve := fakeVerifier{err: ErrOIDCNonceMismatch}
	svc := newTestOIDC(t, &fakeDB{}, true, ex, ve)
	st := LoginState{State: "s", Nonce: "n", CodeVerifier: "v"}

	_, err := svc.Callback(context.Background(), CallbackParams{Code: "c", State: "s"}, st)
	if !errors.Is(err, ErrOIDCNonceMismatch) {
		t.Fatalf("nonce mismatch error = %v, want ErrOIDCNonceMismatch", err)
	}
}

// --- Callback: an unverified email is refused (link/JIT only on verified). ---

func TestCallbackRejectsUnverifiedEmail(t *testing.T) {
	ex := &fakeExchanger{rawIDToken: "tok"}
	ve := fakeVerifier{claims: Claims{Subject: "sub-1", Email: "u@example.test", EmailVerified: false, Nonce: "n"}}
	svc := newTestOIDC(t, &fakeDB{}, true, ex, ve)
	st := LoginState{State: "s", Nonce: "n", CodeVerifier: "v"}

	_, err := svc.Callback(context.Background(), CallbackParams{Code: "c", State: "s"}, st)
	if !errors.Is(err, ErrOIDCEmailUnverified) {
		t.Fatalf("unverified email error = %v, want ErrOIDCEmailUnverified", err)
	}
}

// --- resolveUser: existing identity link short-circuits (no email lookup). ---

func TestCallbackExistingIdentityLink(t *testing.T) {
	// First QueryRow (identity lookup) returns the linked user; then last_login
	// update + session insert follow.
	db := &fakeDB{rows: []fakeRow{
		{vals: []any{"user-existing"}}, // user_identities lookup
		{vals: []any{"sess-1"}},        // sessions insert returning id
	}}
	ex := &fakeExchanger{rawIDToken: "tok"}
	ve := fakeVerifier{claims: Claims{Subject: "sub-1", Email: "u@example.test", EmailVerified: true, Nonce: "n"}}
	svc := newTestOIDC(t, db, true, ex, ve)

	sess, err := svc.Callback(context.Background(), CallbackParams{Code: "c", State: "s"}, LoginState{State: "s", Nonce: "n", CodeVerifier: "v"})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if sess.UserID != "user-existing" {
		t.Fatalf("session user = %q, want user-existing", sess.UserID)
	}
	if sess.Token == "" {
		t.Fatal("session token empty")
	}
	// No user-by-email lookup and no identity insert should have run.
	for _, e := range db.execs {
		if strings.Contains(e, "insert into user_identities") {
			t.Fatalf("unexpected identity insert for an already-linked user: %v", db.execs)
		}
	}
}

// --- resolveUser: no identity, but a user with the verified email exists ->
// link the identity to that user (account linking). ---

func TestCallbackLinksExistingUserByVerifiedEmail(t *testing.T) {
	db := &fakeDB{rows: []fakeRow{
		{err: pgx.ErrNoRows},           // identity lookup: none
		{vals: []any{"user-by-email"}}, // users-by-email lookup: found
		{vals: []any{"sess-1"}},        // sessions insert returning id
	}}
	ex := &fakeExchanger{rawIDToken: "tok"}
	ve := fakeVerifier{claims: Claims{Subject: "sub-2", Email: "u@example.test", EmailVerified: true, Nonce: "n"}}
	svc := newTestOIDC(t, db, true, ex, ve)

	sess, err := svc.Callback(context.Background(), CallbackParams{Code: "c", State: "s"}, LoginState{State: "s", Nonce: "n", CodeVerifier: "v"})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if sess.UserID != "user-by-email" {
		t.Fatalf("session user = %q, want user-by-email", sess.UserID)
	}
	if !containsExec(db.execs, "insert into user_identities") {
		t.Fatalf("expected identity link insert, got execs: %v", db.execs)
	}
	if containsExec(db.execs, "insert into users") {
		t.Fatalf("must not create a new user when one matches the verified email: %v", db.execs)
	}
}

// --- resolveUser: no identity and no user, JIT disabled -> refuse. ---

func TestCallbackRefusesWhenJITDisabledAndNoUser(t *testing.T) {
	db := &fakeDB{rows: []fakeRow{
		{err: pgx.ErrNoRows}, // identity lookup: none
		{err: pgx.ErrNoRows}, // users-by-email lookup: none
	}}
	ex := &fakeExchanger{rawIDToken: "tok"}
	ve := fakeVerifier{claims: Claims{Subject: "sub-3", Email: "new@example.test", EmailVerified: true, Nonce: "n"}}
	svc := newTestOIDC(t, db, false /* JIT disabled */, ex, ve)

	_, err := svc.Callback(context.Background(), CallbackParams{Code: "c", State: "s"}, LoginState{State: "s", Nonce: "n", CodeVerifier: "v"})
	if !errors.Is(err, ErrOIDCUserNotProvisioned) {
		t.Fatalf("err = %v, want ErrOIDCUserNotProvisioned", err)
	}
	if containsExec(db.execs, "insert into users") {
		t.Fatalf("JIT disabled must not create a user: %v", db.execs)
	}
}

// --- resolveUser: no identity and no user, JIT enabled -> create user+identity. ---

func TestCallbackJITCreatesUserWhenEnabled(t *testing.T) {
	db := &fakeDB{rows: []fakeRow{
		{err: pgx.ErrNoRows},      // identity lookup: none
		{err: pgx.ErrNoRows},      // users-by-email lookup: none
		{vals: []any{"user-jit"}}, // insert users returning id
		{vals: []any{"sess-1"}},   // sessions insert returning id
	}}
	ex := &fakeExchanger{rawIDToken: "tok"}
	ve := fakeVerifier{claims: Claims{Subject: "sub-4", Email: "new@example.test", EmailVerified: true, Name: "New User", Nonce: "n"}}
	svc := newTestOIDC(t, db, true, ex, ve)

	sess, err := svc.Callback(context.Background(), CallbackParams{Code: "c", State: "s"}, LoginState{State: "s", Nonce: "n", CodeVerifier: "v"})
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if sess.UserID != "user-jit" {
		t.Fatalf("session user = %q, want user-jit", sess.UserID)
	}
	if !containsExec(db.execs, "insert into user_identities") {
		t.Fatalf("expected identity link insert after JIT, got execs: %v", db.execs)
	}
}

func containsExec(execs []string, substr string) bool {
	for _, e := range execs {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func queryParam(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u.Query().Get(key)
}

func assertQueryParam(t *testing.T, raw, key, want string) {
	t.Helper()
	if got := queryParam(t, raw, key); got != want {
		t.Fatalf("query param %q = %q, want %q (url=%s)", key, got, want, redact(raw))
	}
}

func redact(raw string) string {
	// Never let a test failure print a raw client secret if one leaks into a URL.
	return strings.SplitN(raw, "client_secret", 2)[0]
}
