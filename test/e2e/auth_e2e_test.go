//go:build e2e

// STORY-03.1 golden path: control-plane user accounts and sessions against the
// REAL control-plane Postgres (up via `mise run up`), no mocks. It drives the
// real auth.Service (FromPool) through:
//   - signup (argon2id password hash persisted; plaintext never stored),
//   - login (session created; the cookie token is stored only as a sha256 hash),
//   - session lookup (resolves the session; unknown token rejected),
//   - logout (session revoked; the token no longer authenticates),
//
// and separately proves the lockout policy (SPEC-09 §3): 10 failed logins lock
// the account so even the correct password is refused within the window.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/cp/auth"
)

// uniqueEmail returns a per-run unique lowercase email so reruns do not collide.
func uniqueEmail(t *testing.T) string {
	t.Helper()
	return "auth-" + strings.ReplaceAll(mustSuffix(t), "-", "") + "@example.test"
}

func TestAuthSignupLoginSessionLogoutGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)
	svc := auth.NewService(auth.FromPool(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	email := uniqueEmail(t)
	const password = "a-sufficiently-strong-password"

	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM users WHERE email = '%s'", email))
	})

	// --- Signup: creates a user with an argon2id hash. ---
	userID, err := svc.Signup(ctx, email, password)
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	if userID == "" {
		t.Fatal("signup returned empty user id")
	}

	// The stored hash is argon2id and never the plaintext (SPEC-09 §3).
	storedHash := psqlScalar(t, fmt.Sprintf("select password_hash from users where email = '%s'", email))
	if !strings.HasPrefix(storedHash, "$argon2id$") {
		t.Fatalf("stored password_hash is not argon2id: %q", storedHash)
	}
	if strings.Contains(storedHash, password) {
		t.Fatal("stored password_hash leaked the plaintext")
	}

	// A duplicate signup is rejected.
	if _, err := svc.Signup(ctx, email, password); err == nil {
		t.Fatal("duplicate signup succeeded; want email-taken error")
	}

	// --- Login: creates a session; the cookie token is stored only hashed. ---
	sess, err := svc.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if sess.Token == "" || sess.CSRFToken == "" {
		t.Fatal("login returned empty token or csrf token")
	}

	// The stored token_hash is the sha256 of the cookie token, not the token
	// itself (pgcrypto digest() proves the hashing rather than trusting the app).
	hashed := psqlScalar(t, fmt.Sprintf(
		"select count(*) from sessions where token_hash = digest('%s','sha256')", sess.Token))
	if hashed != "1" {
		t.Fatalf("no session row whose token_hash = sha256(token); got count %q (token not hashed as expected)", hashed)
	}
	// Exactly one live session exists for the user.
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from sessions s join users u on u.id = s.user_id where u.email = '%s' and s.revoked_at is null", email)); n != "1" {
		t.Fatalf("live session count = %q, want 1", n)
	}

	// --- Lookup: resolves the session; a bogus token does not. ---
	got, err := svc.Lookup(ctx, sess.Token)
	if err != nil {
		t.Fatalf("lookup valid token: %v", err)
	}
	if got.UserID != userID {
		t.Fatalf("lookup resolved user %q, want %q", got.UserID, userID)
	}
	if _, err := svc.Lookup(ctx, "not-a-real-token"); err == nil {
		t.Fatal("lookup of a bogus token succeeded")
	}

	// --- Logout: revokes the session so the token stops authenticating. ---
	if err := svc.Revoke(ctx, sess.Token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Lookup(ctx, sess.Token); err == nil {
		t.Fatal("session still valid after logout")
	}
	if n := psqlScalar(t, fmt.Sprintf(
		"select count(*) from sessions s join users u on u.id = s.user_id where u.email = '%s' and s.revoked_at is not null", email)); n != "1" {
		t.Fatalf("revoked session count = %q, want 1", n)
	}
}

func TestAuthLockoutAfterFailedLogins(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)
	svc := auth.NewService(auth.FromPool(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	email := uniqueEmail(t)
	const password = "a-sufficiently-strong-password"
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM users WHERE email = '%s'", email))
	})

	if _, err := svc.Signup(ctx, email, password); err != nil {
		t.Fatalf("signup: %v", err)
	}

	// 9 wrong attempts: still invalid-credentials, not yet locked.
	for i := 0; i < 9; i++ {
		if _, err := svc.Login(ctx, email, "wrong"); err == nil {
			t.Fatalf("attempt %d: wrong password succeeded", i+1)
		}
	}
	// The 10th wrong attempt trips the lockout (SPEC-09 §3: 10 failures / 15 min).
	if _, err := svc.Login(ctx, email, "wrong"); err == nil {
		t.Fatal("10th wrong attempt did not error")
	}

	// A lock deadline is now recorded in the future.
	locked := psqlScalar(t, fmt.Sprintf(
		"select locked_until > now() from users where email = '%s'", email))
	if locked != "t" {
		t.Fatalf("locked_until is not in the future after lockout (got %q)", locked)
	}

	// Even the CORRECT password is refused while locked.
	if _, err := svc.Login(ctx, email, password); err == nil {
		t.Fatal("correct password accepted while account is locked")
	}

	// Simulate the window elapsing: clear the lock deadline into the past. The
	// correct password then succeeds and the counters reset (fail-open after the
	// window, per policy).
	user := hostPort("POSTGRES_USER", "rag")
	runPsql(t, user, "control_plane", fmt.Sprintf(
		"UPDATE users SET locked_until = now() - interval '1 minute' WHERE email = '%s'", email))
	if _, err := svc.Login(ctx, email, password); err != nil {
		t.Fatalf("login after lockout window elapsed: %v", err)
	}
	reset := psqlScalar(t, fmt.Sprintf(
		"select failed_login_count from users where email = '%s'", email))
	if reset != "0" {
		t.Fatalf("failed_login_count = %q after successful login, want 0", reset)
	}
}
