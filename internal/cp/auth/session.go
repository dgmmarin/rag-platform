package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Session is a live server-side session. Token is the plaintext cookie value —
// returned only from Login/createSession so it can be set as a cookie once; it
// is never re-read from the database (only its hash is stored). CSRFToken backs
// the double-submit check on mutating routes.
type Session struct {
	ID        string
	UserID    string
	Token     string // plaintext cookie value; only ever returned at creation
	CSRFToken string
	ExpiresAt time.Time
}

// createSession mints a session token + CSRF token, stores the token's hash with
// a 12 h idle deadline (SPEC-09 §3), and returns the plaintext for the cookie.
func (s *Service) createSession(ctx context.Context, userID string, now time.Time) (Session, error) {
	token, err := newSessionToken()
	if err != nil {
		return Session{}, err
	}
	csrf, err := newCSRFToken()
	if err != nil {
		return Session{}, err
	}
	expires := now.Add(sessionIdleTimeout)

	var id string
	err = s.DB.QueryRow(ctx,
		`insert into sessions (user_id, token_hash, csrf_token, created_at, idle_expires_at)
		 values ($1, $2, $3, $4, $5) returning id::text`,
		userID, hashToken(token), csrf, now, expires).Scan(&id)
	if err != nil {
		return Session{}, fmt.Errorf("auth: create session: %w", err)
	}
	return Session{ID: id, UserID: userID, Token: token, CSRFToken: csrf, ExpiresAt: expires}, nil
}

// Lookup resolves a plaintext cookie token to its session, enforcing the idle
// timeout and revocation. A hit advances the idle deadline (sliding window). A
// miss (unknown, revoked, or expired token) returns ErrNoSession — never
// distinguishing why, to avoid an enumeration oracle. The returned Session has
// an empty Token (the plaintext is never re-read from storage).
func (s *Service) Lookup(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	now := s.now()

	var sess Session
	var expires time.Time
	err := s.DB.QueryRow(ctx,
		`select id::text, user_id::text, csrf_token, idle_expires_at
		 from sessions
		 where token_hash = $1 and revoked_at is null and idle_expires_at > $2`,
		hashToken(token), now).Scan(&sess.ID, &sess.UserID, &sess.CSRFToken, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNoSession
		}
		return Session{}, fmt.Errorf("auth: look up session: %w", err)
	}

	// Slide the idle window forward.
	newExpires := now.Add(sessionIdleTimeout)
	if _, err := s.DB.Exec(ctx,
		`update sessions set idle_expires_at = $2 where id = $1`, sess.ID, newExpires); err != nil {
		return Session{}, fmt.Errorf("auth: refresh session: %w", err)
	}
	sess.ExpiresAt = newExpires
	return sess, nil
}

// Revoke marks the session for a plaintext token revoked (logout). It is
// idempotent: revoking an unknown or already-revoked token is a no-op success,
// so a double logout does not error.
func (s *Service) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.DB.Exec(ctx,
		`update sessions set revoked_at = now() where token_hash = $1 and revoked_at is null`,
		hashToken(token)); err != nil {
		return fmt.Errorf("auth: revoke session: %w", err)
	}
	return nil
}

// poolDB adapts *pgxpool.Pool to the auth DB interface (its Row/CommandTag types
// satisfy the narrow interfaces the service scans against).
type poolDB struct{ pool *pgxpool.Pool }

// FromPool wraps a pgx pool as the auth service's DB.
func FromPool(pool *pgxpool.Pool) DB { return poolDB{pool: pool} }

func (p poolDB) Exec(ctx context.Context, sql string, args ...any) (pgconnTag, error) {
	tag, err := p.pool.Exec(ctx, sql, args...)
	return tag, err
}

func (p poolDB) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return p.pool.QueryRow(ctx, sql, args...)
}

// Query adapts the pool's multi-row query to the membership Rows interface
// (pgx.Rows already satisfies it).
func (p poolDB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	return p.pool.Query(ctx, sql, args...)
}

// MembershipFromPool wraps a pgx pool as the membership service's DB.
func MembershipFromPool(pool *pgxpool.Pool) MembershipDB { return poolDB{pool: pool} }
