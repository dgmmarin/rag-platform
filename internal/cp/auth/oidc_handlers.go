package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
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

// OIDCHandlers are the HTTP handlers for the OIDC login flow. Secure controls the
// cookie Secure attribute (true in production over TLS).
type OIDCHandlers struct {
	Service *OIDCService
	Secure  bool
}

// Start redirects the browser to the provider's authorization endpoint and sets
// the state cookie carrying the state/nonce/PKCE verifier for Callback.
func (h *OIDCHandlers) Start(w http.ResponseWriter, r *http.Request) {
	authURL, st, err := h.Service.AuthCodeURL(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oidc unavailable")
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
		writeError(w, http.StatusBadRequest, "missing or expired login state")
		return
	}
	st, err := decodeLoginState(c.Value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid login state")
		return
	}
	// Clear the single-use state cookie regardless of outcome.
	http.SetCookie(w, h.clearStateCookie())

	params := CallbackParams{Code: r.URL.Query().Get("code"), State: r.URL.Query().Get("state")}
	sess, err := h.Service.Callback(r.Context(), params, st)
	switch {
	case errors.Is(err, ErrOIDCStateMismatch), errors.Is(err, ErrOIDCNonceMismatch):
		writeError(w, http.StatusBadRequest, "login validation failed")
		return
	case errors.Is(err, ErrOIDCEmailUnverified):
		writeError(w, http.StatusForbidden, "email not verified with identity provider")
		return
	case errors.Is(err, ErrOIDCUserNotProvisioned):
		writeError(w, http.StatusForbidden, "no account for this identity")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}

	// Same session cookie + CSRF response shape as password login (do not fork
	// session handling).
	sh := &Handlers{Service: h.Service.Auth, Secure: h.Secure}
	http.SetCookie(w, sh.sessionCookie(sess.Token))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"csrf_token": sess.CSRFToken})
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
