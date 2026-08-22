package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Cookie/header names for the session and CSRF double-submit token (SPEC-09 §3).
const (
	SessionCookieName = "rag_session"
	CSRFHeaderName    = "X-CSRF-Token"
)

// ctxKey is the private context key type for the resolved session.
type ctxKey int

const sessionCtxKey ctxKey = iota

// SessionFrom returns the session resolved by RequireSession/Middleware, if any.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionCtxKey).(Session)
	return s, ok
}

// Handlers are the HTTP entry points for password auth (SPEC-02 §3). They are
// unit-tested with httptest here; STORY-04.1 mounts them on the public router.
// Secure controls the cookie Secure attribute (true in production over TLS,
// false for plaintext local dev).
type Handlers struct {
	Service *Service
	Secure  bool
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Signup registers a new email/password user.
func (h *Handlers) Signup(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	_, err := h.Service.Signup(r.Context(), c.Email, c.Password)
	switch {
	case errors.Is(err, ErrEmailTaken):
		writeError(w, http.StatusConflict, "email already registered")
	case errors.Is(err, ErrWeakPassword):
		writeError(w, http.StatusBadRequest, "password too weak")
	case errors.Is(err, ErrInvalidCredentials):
		writeError(w, http.StatusBadRequest, "email required")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "signup failed")
	default:
		w.WriteHeader(http.StatusCreated)
	}
}

// Login verifies credentials and, on success, sets the session cookie and
// returns the CSRF token for the client to echo on mutating requests. The raw
// session token is only ever delivered via the HttpOnly cookie, never the body.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	var c credentials
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	sess, err := h.Service.Login(r.Context(), c.Email, c.Password)
	switch {
	case errors.Is(err, ErrAccountLocked):
		writeError(w, http.StatusTooManyRequests, "account locked; try again later")
		return
	case errors.Is(err, ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}

	http.SetCookie(w, h.sessionCookie(sess.Token))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"csrf_token": sess.CSRFToken})
}

// Logout revokes the current session and clears the cookie. It is idempotent.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if err := h.Service.Revoke(r.Context(), c.Value); err != nil {
			writeError(w, http.StatusInternalServerError, "logout failed")
			return
		}
	}
	http.SetCookie(w, h.clearCookie())
	w.WriteHeader(http.StatusNoContent)
}

// RequireSession resolves the session cookie and injects the session into the
// request context, or responds 401 if there is no valid session. The inner
// handler runs only for authenticated requests.
func (h *Handlers) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(SessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		sess, err := h.Service.Lookup(r.Context(), c.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		ctx := context.WithValue(r.Context(), sessionCtxKey, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// CSRF enforces the double-submit check on mutating methods: the request must
// carry a CSRF header matching the session's token. Safe methods (GET/HEAD/
// OPTIONS) pass through. It must run inside RequireSession (it reads the session
// from the context).
func (h *Handlers) CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		sess, ok := SessionFrom(r.Context())
		if !ok {
			writeError(w, http.StatusForbidden, "csrf check failed")
			return
		}
		if !csrfMatch(r.Header.Get(CSRFHeaderName), sess.CSRFToken) {
			writeError(w, http.StatusForbidden, "csrf check failed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// sessionCookie builds the session cookie (SPEC-09 §3): HttpOnly, SameSite=Lax,
// Secure in production, scoped to the whole site. It is a session cookie (no
// Expires): the authoritative 12 h idle lifetime is enforced server-side by the
// session store, so a stale cookie simply fails Lookup.
func (h *Handlers) sessionCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func (h *Handlers) clearCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.Secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
