package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// HTTP entry points for OIDC login (STORY-03.2). Like the password handlers in
// middleware.go, they are unit-tested with httptest here; STORY-04.1 mounts them
// on the public router. Start begins the authorization-code+PKCE flow and stashes
// the per-request state/nonce/verifier in a short-lived cookie; Callback validates
// the return, mints a session (the same store as password login), and sets the
// session cookie. No token or client secret is ever logged.

// oidcStateCookieName holds the encoded LoginState between Start and Callback. It
// is HttpOnly + SameSite=Lax and short-lived; the `state` query parameter must
// match the state inside it, which a cross-site attacker cannot read or set.
const oidcStateCookieName = "rag_oidc_state"

// oidcStateTTLSeconds bounds how long a login attempt may take before the state
// cookie expires (5 minutes is ample for the redirect round trip).
const oidcStateTTLSeconds = 300

// Default browser redirect targets for the OIDC flow (ISSUE-0058). Both are SPA
// paths on the Next origin; a relative path is origin-agnostic, so the same binary
// works across environments (ADR-0073). SuccessURL is overridable per deployment.
const (
	defaultOIDCSuccessURL = "/admin"
	defaultOIDCFailureURL = "/admin/login"
)

// OIDCHandlers are the HTTP handlers for the OIDC login flow. Secure controls the
// cookie Secure attribute (true in production over TLS).
//
// The OIDC flow is driven by top-level BROWSER navigation, so Start and Callback
// terminate the response with a redirect, never a JSON body (ISSUE-0058): on
// success Callback sets the session cookie and 303-redirects into the SPA
// (SuccessURL); on any failure it 303-redirects to FailureURL with an ?error=<code>
// so the login page can show a message. The SPA then hydrates via GET /v1/auth/me,
// which returns the CSRF token — so no fork of session handling is needed (ADR-0020).
type OIDCHandlers struct {
	Service *OIDCService
	Secure  bool
	// SuccessURL is where Callback redirects after a session is minted (default
	// /admin). FailureURL is the login page a failure redirects to (default
	// /admin/login). Empty falls back to the defaults.
	SuccessURL string
	FailureURL string
}

// successURL is the post-login redirect target, or the default when unset.
func (h *OIDCHandlers) successURL() string {
	if h.SuccessURL != "" {
		return h.SuccessURL
	}
	return defaultOIDCSuccessURL
}

// failureURL is the login page plus an ?error=<code> query, so a browser
// navigation never dead-ends on a raw error body.
func (h *OIDCHandlers) failureURL(code string) string {
	base := h.FailureURL
	if base == "" {
		base = defaultOIDCFailureURL
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "error=" + url.QueryEscape(code)
}

// Start redirects the browser to the provider's authorization endpoint and sets
// the state cookie carrying the state/nonce/PKCE verifier for Callback.
func (h *OIDCHandlers) Start(w http.ResponseWriter, r *http.Request) {
	authURL, st, err := h.Service.AuthCodeURL(r.Context())
	if err != nil {
		http.Redirect(w, r, h.failureURL("unavailable"), http.StatusSeeOther)
		return
	}
	http.SetCookie(w, h.stateCookie(encodeLoginState(st)))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// Callback completes the flow: it recovers the LoginState from the cookie,
// validates state/nonce/PKCE via the service, links or JIT-creates the user, and
// sets the session cookie. The state cookie is cleared on the way out.
func (h *OIDCHandlers) Callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(oidcStateCookieName)
	if err != nil {
		http.Redirect(w, r, h.failureURL("invalid_state"), http.StatusSeeOther)
		return
	}
	st, err := decodeLoginState(c.Value)
	if err != nil {
		http.Redirect(w, r, h.failureURL("invalid_state"), http.StatusSeeOther)
		return
	}
	// Clear the single-use state cookie regardless of outcome.
	http.SetCookie(w, h.clearStateCookie())

	params := CallbackParams{Code: r.URL.Query().Get("code"), State: r.URL.Query().Get("state")}
	sess, err := h.Service.Callback(r.Context(), params, st)
	switch {
	case errors.Is(err, ErrOIDCStateMismatch), errors.Is(err, ErrOIDCNonceMismatch):
		http.Redirect(w, r, h.failureURL("login_failed"), http.StatusSeeOther)
		return
	case errors.Is(err, ErrOIDCEmailUnverified):
		http.Redirect(w, r, h.failureURL("email_unverified"), http.StatusSeeOther)
		return
	case errors.Is(err, ErrOIDCUserNotProvisioned):
		http.Redirect(w, r, h.failureURL("not_provisioned"), http.StatusSeeOther)
		return
	case err != nil:
		http.Redirect(w, r, h.failureURL("login_failed"), http.StatusSeeOther)
		return
	}

	// Set the SAME session cookie as password login (do not fork session handling,
	// ADR-0020), then redirect the browser into the SPA. The CSRF token is not
	// returned here; the SPA reads it from GET /v1/auth/me on hydration.
	sh := &Handlers{Service: h.Service.Auth, Secure: h.Secure}
	http.SetCookie(w, sh.sessionCookie(sess.Token))
	http.Redirect(w, r, h.successURL(), http.StatusSeeOther)
}

func (h *OIDCHandlers) stateCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   oidcStateTTLSeconds,
	}
}

func (h *OIDCHandlers) clearStateCookie() *http.Cookie {
	return &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// encodeLoginState serialises a LoginState for the state cookie (base64url JSON).
// The values it carries (state, nonce, verifier) are single-use secrets bound to
// this one attempt; the security property is that a cross-site attacker can
// neither read nor set this HttpOnly cookie, so the `state` query parameter
// cannot be forged to match it.
func encodeLoginState(st LoginState) string {
	b, _ := json.Marshal(st)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeLoginState(s string) (LoginState, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return LoginState{}, err
	}
	var st LoginState
	if err := json.Unmarshal(b, &st); err != nil {
		return LoginState{}, err
	}
	if st.State == "" || st.Nonce == "" || st.CodeVerifier == "" {
		return LoginState{}, errors.New("auth: incomplete oidc state")
	}
	return st, nil
}
