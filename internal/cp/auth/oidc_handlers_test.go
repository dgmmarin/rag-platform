package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func newTestOIDCHandlers(db DB, jit bool, ex Exchanger, ve Verifier) *OIDCHandlers {
	svc := &OIDCService{
		Auth:      &Service{DB: db, Lockout: DefaultLockoutPolicy(), Now: func() time.Time { return time.Unix(1700000000, 0).UTC() }},
		Config:    OIDCConfig{Issuer: "https://idp.example", ClientID: "c", RedirectURL: "https://app/callback", JITProvisioning: jit},
		Exchanger: ex,
		Verifier:  ve,
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	return &OIDCHandlers{Service: svc, Secure: false}
}

// TestStartSetsStateCookieAndRedirects proves Start redirects to the provider and
// stashes the per-request LoginState in a short-lived HttpOnly cookie.
func TestStartSetsStateCookieAndRedirects(t *testing.T) {
	h := newTestOIDCHandlers(nil, true, &fakeExchanger{}, fakeVerifier{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/start", nil)

	h.Start(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://idp.example/authorize") {
		t.Fatalf("redirect location %q not to provider", loc)
	}
	var stateCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == oidcStateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("no oidc state cookie set")
	}
	if !stateCookie.HttpOnly {
		t.Fatal("oidc state cookie must be HttpOnly")
	}
}

// TestCallbackSetsSessionCookie proves a valid callback (matching state cookie)
// mints the SAME session cookie as password login and returns the CSRF token.
func TestCallbackSetsSessionCookie(t *testing.T) {
	db := &fakeDB{rows: []fakeRow{
		{err: pgx.ErrNoRows},      // identity lookup: none
		{err: pgx.ErrNoRows},      // user-by-email: none
		{vals: []any{"user-jit"}}, // insert users returning id
		{vals: []any{"sess-1"}},   // sessions insert returning id
	}}
	ex := &fakeExchanger{rawIDToken: "tok"}
	ve := fakeVerifier{claims: Claims{Subject: "sub", Email: "u@example.test", EmailVerified: true, Nonce: "the-nonce"}}
	h := newTestOIDCHandlers(db, true, ex, ve)

	// Simulate the state cookie the Start step set (encoded LoginState).
	st := LoginState{State: "the-state", Nonce: "the-nonce", CodeVerifier: "the-verifier"}
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state=the-state", nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: encodeLoginState(st)})

	rr := httptest.NewRecorder()
	h.Callback(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == SessionCookieName {
			got = c
		}
	}
	if got == nil || got.Value == "" {
		t.Fatal("callback did not set a session cookie")
	}
	if !strings.Contains(rr.Body.String(), "csrf_token") {
		t.Fatalf("callback body missing csrf_token: %s", rr.Body.String())
	}
}

// TestCallbackRejectsMissingStateCookie proves a callback without the state
// cookie (e.g. a forged request) is refused with 400 and no session.
func TestCallbackRejectsMissingStateCookie(t *testing.T) {
	h := newTestOIDCHandlers(&fakeDB{}, true, &fakeExchanger{rawIDToken: "tok"}, fakeVerifier{})
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state=the-state", nil)
	rr := httptest.NewRecorder()

	h.Callback(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// TestCallbackJITDisabledReturns403 proves an unprovisioned user with JIT off is
// refused with 403.
func TestCallbackJITDisabledReturns403(t *testing.T) {
	db := &fakeDB{rows: []fakeRow{
		{err: pgx.ErrNoRows}, // identity lookup
		{err: pgx.ErrNoRows}, // user-by-email
	}}
	ex := &fakeExchanger{rawIDToken: "tok"}
	ve := fakeVerifier{claims: Claims{Subject: "sub", Email: "nobody@example.test", EmailVerified: true, Nonce: "n"}}
	h := newTestOIDCHandlers(db, false, ex, ve)

	st := LoginState{State: "s", Nonce: "n", CodeVerifier: "v"}
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state=s", nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: encodeLoginState(st)})
	rr := httptest.NewRecorder()

	h.Callback(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}
