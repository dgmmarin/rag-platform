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

// TestCallbackSetsSessionCookieAndRedirects proves a valid callback (matching state
// cookie) mints the SAME session cookie as password login and 303-redirects the
// browser into the SPA (ISSUE-0058), with no JSON body — the SPA reads the CSRF
// token from GET /v1/auth/me on hydration.
func TestCallbackSetsSessionCookieAndRedirects(t *testing.T) {
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

	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body=%s, want 303", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != defaultOIDCSuccessURL {
		t.Fatalf("redirect location = %q, want %q", loc, defaultOIDCSuccessURL)
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
	if strings.Contains(rr.Body.String(), "csrf_token") {
		t.Fatalf("callback must not return a JSON body on redirect: %s", rr.Body.String())
	}
}

// TestCallbackSuccessURLOverride proves a configured SuccessURL is the redirect
// target (ISSUE-0058: the post-login target is per-deployment configurable).
func TestCallbackSuccessURLOverride(t *testing.T) {
	db := &fakeDB{rows: []fakeRow{
		{err: pgx.ErrNoRows},
		{err: pgx.ErrNoRows},
		{vals: []any{"user-jit"}},
		{vals: []any{"sess-1"}},
	}}
	ex := &fakeExchanger{rawIDToken: "tok"}
	ve := fakeVerifier{claims: Claims{Subject: "sub", Email: "u@example.test", EmailVerified: true, Nonce: "the-nonce"}}
	h := newTestOIDCHandlers(db, true, ex, ve)
	h.SuccessURL = "/console/home"

	st := LoginState{State: "the-state", Nonce: "the-nonce", CodeVerifier: "the-verifier"}
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state=the-state", nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookieName, Value: encodeLoginState(st)})
	rr := httptest.NewRecorder()

	h.Callback(rr, req)
	if loc := rr.Header().Get("Location"); loc != "/console/home" {
		t.Fatalf("redirect location = %q, want %q", loc, "/console/home")
	}
}

// TestCallbackFailuresRedirectToLogin proves every failure path 303-redirects to
// the login page with an ?error=<code>, never a raw JSON body (ISSUE-0058), and
// mints no session.
func TestCallbackFailuresRedirectToLogin(t *testing.T) {
	// Missing state cookie (e.g. a forged request) → invalid_state.
	t.Run("missing state cookie", func(t *testing.T) {
		h := newTestOIDCHandlers(&fakeDB{}, true, &fakeExchanger{rawIDToken: "tok"}, fakeVerifier{})
		req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=abc&state=the-state", nil)
		rr := httptest.NewRecorder()

		h.Callback(rr, req)

		assertLoginRedirect(t, rr, "invalid_state")
	})

	// Unprovisioned user with JIT off → not_provisioned.
	t.Run("jit disabled", func(t *testing.T) {
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

		assertLoginRedirect(t, rr, "not_provisioned")
	})
}

// assertLoginRedirect asserts rr is a 303 to the login page carrying ?error=code
// and set no session cookie.
func assertLoginRedirect(t *testing.T, rr *httptest.ResponseRecorder, code string) {
	t.Helper()
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rr.Code)
	}
	wantLoc := defaultOIDCFailureURL + "?error=" + code
	if loc := rr.Header().Get("Location"); loc != wantLoc {
		t.Fatalf("redirect location = %q, want %q", loc, wantLoc)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			t.Fatal("failure path must not mint a session cookie")
		}
	}
}
