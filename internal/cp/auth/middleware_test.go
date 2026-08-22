package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// memStore is an in-memory DB implementing the auth DB interface, enough to
// drive the HTTP layer end to end without a real Postgres (the real round trip
// is proven in the e2e). It supports the exact statements the service issues.
type memStore struct {
	users    map[string]*memUser // by email
	usersID  map[string]*memUser // by id
	sessions map[string]*memSession
	nextID   int
}

type memUser struct {
	id           string
	email        string
	passwordHash string
	failedCount  int
	lockedUntil  *time.Time
}

type memSession struct {
	id        string
	userID    string
	tokenHash string
	csrf      string
	expires   time.Time
	revoked   bool
}

func newMemStore() *memStore {
	return &memStore{users: map[string]*memUser{}, usersID: map[string]*memUser{}, sessions: map[string]*memSession{}}
}

func (m *memStore) id() string {
	m.nextID++
	return "id-" + itoa(m.nextID)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (m *memStore) Exec(_ context.Context, sql string, args ...any) (pgconnTag, error) {
	switch {
	case strings.Contains(sql, "update users set failed_login_count = 0"):
		u := m.usersID[args[0].(string)]
		u.failedCount = 0
		u.lockedUntil = nil
	case strings.Contains(sql, "update users set failed_login_count = $2, locked_until = $3"):
		u := m.usersID[args[0].(string)]
		u.failedCount = args[1].(int)
		lu := args[2].(time.Time)
		u.lockedUntil = &lu
	case strings.Contains(sql, "update users set failed_login_count = $2 where"):
		u := m.usersID[args[0].(string)]
		u.failedCount = args[1].(int)
	case strings.Contains(sql, "update sessions set idle_expires_at"):
		s := m.sessionByID(args[0].(string))
		s.expires = args[1].(time.Time)
	case strings.Contains(sql, "update sessions set revoked_at"):
		th := string(args[0].([]byte))
		for _, s := range m.sessions {
			if s.tokenHash == th {
				s.revoked = true
			}
		}
	}
	return fakeTag{n: 1}, nil
}

func (m *memStore) sessionByID(id string) *memSession {
	for _, s := range m.sessions {
		if s.id == id {
			return s
		}
	}
	return nil
}

func (m *memStore) QueryRow(_ context.Context, sql string, args ...any) Row {
	switch {
	case strings.Contains(sql, "insert into users"):
		email := args[0].(string)
		if _, exists := m.users[email]; exists {
			return fakeRow{err: uniqueErr{}}
		}
		u := &memUser{id: m.id(), email: email, passwordHash: args[1].(string)}
		m.users[email] = u
		m.usersID[u.id] = u
		return fakeRow{vals: []any{u.id}}
	case strings.Contains(sql, "from users where email"):
		u, ok := m.users[args[0].(string)]
		if !ok {
			return fakeRow{err: errNoRows{}}
		}
		var lu any
		if u.lockedUntil != nil {
			lu = *u.lockedUntil
		}
		return fakeRow{vals: []any{u.id, u.passwordHash, u.failedCount, lu}}
	case strings.Contains(sql, "insert into sessions"):
		s := &memSession{
			id: m.id(), userID: args[0].(string), tokenHash: string(args[1].([]byte)),
			csrf: args[2].(string), expires: args[4].(time.Time),
		}
		m.sessions[s.id] = s
		return fakeRow{vals: []any{s.id}}
	case strings.Contains(sql, "from sessions"):
		th := string(args[0].([]byte))
		now := args[1].(time.Time)
		for _, s := range m.sessions {
			if s.tokenHash == th && !s.revoked && s.expires.After(now) {
				return fakeRow{vals: []any{s.id, s.userID, s.csrf, s.expires}}
			}
		}
		return fakeRow{err: errNoRows{}}
	}
	return fakeRow{err: errNoRows{}}
}

// errNoRows / uniqueErr let the mem store surface the two error shapes the
// service maps (pgx.ErrNoRows via Is; unique violation via SQLState).
type errNoRows struct{}

func (errNoRows) Error() string { return "no rows in result set" }
func (errNoRows) Is(target error) bool {
	return target != nil && target.Error() == "no rows in result set"
}

type uniqueErr struct{}

func (uniqueErr) Error() string    { return "unique violation" }
func (uniqueErr) SQLState() string { return "23505" }

func newTestHandlers() (*Handlers, *memStore) {
	store := newMemStore()
	svc := NewService(store)
	return &Handlers{Service: svc, Secure: false}, store
}

// doJSON posts a JSON body and returns the recorder.
func doJSON(t *testing.T, h http.HandlerFunc, method, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// TestSignupLoginLogoutCookieFlow exercises the full HTTP cookie flow against
// the in-memory store: signup, login sets an HttpOnly session cookie, an
// authenticated request resolves the session, and logout revokes it.
func TestSignupLoginLogoutCookieFlow(t *testing.T) {
	h, _ := newTestHandlers()

	// Signup.
	rr := doJSON(t, h.Signup, http.MethodPost, `{"email":"a@b.com","password":"a-strong-password"}`, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("signup: got %d, want 201: %s", rr.Code, rr.Body)
	}

	// Login sets the session cookie with the right attributes.
	rr = doJSON(t, h.Login, http.MethodPost, `{"email":"a@b.com","password":"a-strong-password"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("login: got %d, want 200: %s", rr.Code, rr.Body)
	}
	cookie := findCookie(rr.Result().Cookies(), SessionCookieName)
	if cookie == nil {
		t.Fatal("login did not set a session cookie")
	}
	if !cookie.HttpOnly {
		t.Fatal("session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if strings.Contains(rr.Body.String(), cookie.Value) {
		t.Fatal("login response body leaked the raw session token")
	}

	// An authenticated request resolves the session via the middleware.
	var gotUser string
	protected := h.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess, ok := SessionFrom(r.Context()); ok {
			gotUser = sess.UserID
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(cookie)
	rr2 := httptest.NewRecorder()
	protected.ServeHTTP(rr2, req)
	if rr2.Code != http.StatusNoContent {
		t.Fatalf("protected with valid session: got %d, want 204", rr2.Code)
	}
	if gotUser == "" {
		t.Fatal("session not injected into request context")
	}

	// Logout revokes the session; the cookie no longer authenticates.
	rrOut := doJSON(t, h.Logout, http.MethodPost, "", []*http.Cookie{cookie})
	if rrOut.Code != http.StatusNoContent {
		t.Fatalf("logout: got %d, want 204", rrOut.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(cookie)
	rr3 := httptest.NewRecorder()
	protected.ServeHTTP(rr3, req)
	if rr3.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: got %d, want 401", rr3.Code)
	}
}

// TestRequireSessionRejectsNoCookie proves an unauthenticated request to a
// protected handler is 401 and the inner handler never runs.
func TestRequireSessionRejectsNoCookie(t *testing.T) {
	h, _ := newTestHandlers()
	called := false
	protected := h.RequireSession(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called = true }))
	rr := httptest.NewRecorder()
	protected.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rr.Code)
	}
	if called {
		t.Fatal("inner handler ran without a session")
	}
}

// TestCSRFDoubleSubmit proves mutating requests need a CSRF header matching the
// session's token; GETs are exempt.
func TestCSRFDoubleSubmit(t *testing.T) {
	h, _ := newTestHandlers()
	_ = mustSignup(t, h, "c@d.com", "a-strong-password")
	cookie, csrf := mustLogin(t, h, "c@d.com", "a-strong-password")

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	guarded := h.RequireSession(h.CSRF(inner))

	// Safe method: no CSRF header needed.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	guarded.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET without CSRF header: got %d, want 200", rr.Code)
	}

	// Mutation without a CSRF header: rejected.
	req = httptest.NewRequest(http.MethodPost, "/x", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	guarded.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST without CSRF: got %d, want 403", rr.Code)
	}

	// Mutation with the wrong token: rejected.
	req = httptest.NewRequest(http.MethodPost, "/x", nil)
	req.AddCookie(cookie)
	req.Header.Set(CSRFHeaderName, csrf+"tamper")
	rr = httptest.NewRecorder()
	guarded.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST with wrong CSRF: got %d, want 403", rr.Code)
	}

	// Mutation with the correct token: allowed.
	req = httptest.NewRequest(http.MethodPost, "/x", nil)
	req.AddCookie(cookie)
	req.Header.Set(CSRFHeaderName, csrf)
	rr = httptest.NewRecorder()
	guarded.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST with correct CSRF: got %d, want 200", rr.Code)
	}
}

func mustSignup(t *testing.T, h *Handlers, email, pw string) string {
	t.Helper()
	rr := doJSON(t, h.Signup, http.MethodPost, `{"email":"`+email+`","password":"`+pw+`"}`, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("signup %s: %d %s", email, rr.Code, rr.Body)
	}
	return email
}

func mustLogin(t *testing.T, h *Handlers, email, pw string) (*http.Cookie, string) {
	t.Helper()
	rr := doJSON(t, h.Login, http.MethodPost, `{"email":"`+email+`","password":"`+pw+`"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, rr.Code, rr.Body)
	}
	cookie := findCookie(rr.Result().Cookies(), SessionCookieName)
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	// CSRF token is returned in the JSON body for the SPA to echo back.
	csrf := extractJSON(rr.Body.String(), "csrf_token")
	if csrf == "" {
		t.Fatal("login did not return a csrf_token")
	}
	return cookie, csrf
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// extractJSON is a tiny value reader for `"key":"value"` in a flat JSON object,
// avoiding a struct just for tests.
func extractJSON(body, key string) string {
	marker := `"` + key + `":"`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}
