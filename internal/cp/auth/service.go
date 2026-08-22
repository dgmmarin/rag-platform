package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sentinel errors let callers (HTTP handlers, tests) map failures to responses
// without string matching. Login deliberately collapses "no such user" and
// "wrong password" into ErrInvalidCredentials so it does not disclose which
// emails exist.
var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrAccountLocked      = errors.New("auth: account locked")
	ErrEmailTaken         = errors.New("auth: email already registered")
	ErrNoSession          = errors.New("auth: no such session")
	ErrWeakPassword       = errors.New("auth: password too weak")
)

// minPasswordLen is a floor for signup. The breach-list check SPEC-09 §3 also
// mentions is a follow-up (a wordlist/HIBP integration); this story enforces the
// length floor and leaves a hook for the breach check.
const minPasswordLen = 12

// sessionIdleTimeout is the 12 h idle timeout (SPEC-09 §3).
const sessionIdleTimeout = 12 * time.Hour

// Row is the subset of pgx.Row the service scans. *pgxpool.Pool rows satisfy it.
type Row interface {
	Scan(dest ...any) error
}

// DB is the minimal database surface the auth service needs; *pgxpool.Pool
// satisfies it. Keeping it small lets the branch logic be unit-tested with a
// fake while the golden path is proven end to end against real Postgres.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconnTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// pgconnTag mirrors pgconn.CommandTag's RowsAffected so the service can check
// update effects without importing pgconn into the interface. *pgxpool.Pool
// returns a pgconn.CommandTag, which satisfies this via an adapter (poolDB).
type pgconnTag interface {
	RowsAffected() int64
}

// Clock returns the current time; overridable in tests so lockout windows and
// idle timeouts are deterministic. Zero value uses time.Now.
type Clock func() time.Time

// Service is the control-plane auth service. It owns users and sessions
// (SPEC-02 §2) and is safe to reuse concurrently (it holds no per-request state).
type Service struct {
	DB      DB
	Lockout LockoutPolicy
	Now     Clock
}

// NewService builds a Service with the SPEC-09 §3 defaults.
func NewService(db DB) *Service {
	return &Service{DB: db, Lockout: DefaultLockoutPolicy(), Now: time.Now}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Signup creates a user with an argon2id password hash and returns the user ID.
// The email is normalised (trimmed, lower-cased; the column is citext too).
func (s *Service) Signup(ctx context.Context, email, password string) (string, error) {
	email = normalizeEmail(email)
	if email == "" {
		return "", fmt.Errorf("%w: email required", ErrInvalidCredentials)
	}
	if len(password) < minPasswordLen {
		return "", fmt.Errorf("%w: at least %d characters", ErrWeakPassword, minPasswordLen)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return "", err
	}
	var id string
	err = s.DB.QueryRow(ctx,
		`insert into users (email, password_hash) values ($1, $2) returning id::text`,
		email, hash).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrEmailTaken
		}
		return "", fmt.Errorf("auth: insert user: %w", err)
	}
	return id, nil
}

// userAuthRow is the per-login state read under the lockout check.
type userAuthRow struct {
	id           string
	passwordHash *string
	failedCount  int
	lockedUntil  *time.Time
}

// Login verifies the password and, on success, creates a session. It enforces
// the lockout policy (SPEC-09 §3): while locked it returns ErrAccountLocked
// without checking the password; a wrong password increments the failure count
// and may lock the account; a correct password resets the counters. It never
// discloses whether the email exists.
func (s *Service) Login(ctx context.Context, email, password string) (Session, error) {
	email = normalizeEmail(email)
	now := s.now()

	var u userAuthRow
	err := s.DB.QueryRow(ctx,
		`select id::text, password_hash, failed_login_count, locked_until from users where email = $1`,
		email).Scan(&u.id, &u.passwordHash, &u.failedCount, &u.lockedUntil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown email: same response as a wrong password, no timing oracle
			// worth optimising here beyond not revealing existence.
			return Session{}, ErrInvalidCredentials
		}
		return Session{}, fmt.Errorf("auth: look up user: %w", err)
	}

	if u.lockedUntil != nil && IsLocked(*u.lockedUntil, now) {
		return Session{}, ErrAccountLocked
	}

	// An OIDC-only user (no password set) cannot log in by password.
	if u.passwordHash == nil || *u.passwordHash == "" {
		return Session{}, ErrInvalidCredentials
	}

	ok, err := VerifyPassword(password, *u.passwordHash)
	if err != nil {
		return Session{}, fmt.Errorf("auth: verify password: %w", err)
	}
	if !ok {
		newCount := u.failedCount + 1
		locked, until := s.Lockout.NextFailure(newCount, now)
		if err := s.recordFailure(ctx, u.id, newCount, locked, until); err != nil {
			return Session{}, err
		}
		if locked {
			return Session{}, ErrAccountLocked
		}
		return Session{}, ErrInvalidCredentials
	}

	// Success: clear the counters and stamp last_login_at, then open a session.
	if _, err := s.DB.Exec(ctx,
		`update users set failed_login_count = 0, locked_until = null, last_login_at = $2 where id = $1`,
		u.id, now); err != nil {
		return Session{}, fmt.Errorf("auth: reset login counters: %w", err)
	}
	return s.createSession(ctx, u.id, now)
}

// recordFailure increments the failure count and, when the policy trips, writes
// the lock deadline.
func (s *Service) recordFailure(ctx context.Context, userID string, count int, locked bool, until time.Time) error {
	if locked {
		if _, err := s.DB.Exec(ctx,
			`update users set failed_login_count = $2, locked_until = $3 where id = $1`,
			userID, count, until); err != nil {
			return fmt.Errorf("auth: record locked failure: %w", err)
		}
		return nil
	}
	if _, err := s.DB.Exec(ctx,
		`update users set failed_login_count = $2 where id = $1`, userID, count); err != nil {
		return fmt.Errorf("auth: record failure: %w", err)
	}
	return nil
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// isUniqueViolation reports whether err is a Postgres unique-constraint (23505)
// violation, used to map a duplicate email to ErrEmailTaken.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
