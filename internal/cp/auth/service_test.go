package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// fakeRow returns preset scan values or a preset error, mimicking pgx.Row.
type fakeRow struct {
	vals []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = r.vals[i].(string)
		case **string:
			if r.vals[i] == nil {
				*d = nil
			} else {
				v := r.vals[i].(string)
				*d = &v
			}
		case *int:
			*d = r.vals[i].(int)
		case **time.Time:
			if r.vals[i] == nil {
				*d = nil
			} else {
				v := r.vals[i].(time.Time)
				*d = &v
			}
		case *time.Time:
			*d = r.vals[i].(time.Time)
		default:
			return errors.New("fakeRow: unsupported dest type")
		}
	}
	return nil
}

type fakeTag struct{ n int64 }

func (t fakeTag) RowsAffected() int64 { return t.n }

// fakeExec records executed statements and returns queued rows for QueryRow.
type fakeDB struct {
	rows  []fakeRow // consumed FIFO by QueryRow
	execs []string  // sql of every Exec, in order
}

func (f *fakeDB) Exec(_ context.Context, sql string, _ ...any) (pgconnTag, error) {
	f.execs = append(f.execs, sql)
	return fakeTag{n: 1}, nil
}

func (f *fakeDB) QueryRow(_ context.Context, _ string, _ ...any) Row {
	if len(f.rows) == 0 {
		return fakeRow{err: errors.New("fakeDB: no queued row")}
	}
	r := f.rows[0]
	f.rows = f.rows[1:]
	return r
}

func fixedClock(t time.Time) Clock { return func() time.Time { return t } }

// TestLoginRejectsLockedAccountWithoutCheckingPassword proves a currently-locked
// account returns ErrAccountLocked and never runs the password verify or a
// session insert (SPEC-09 §3).
func TestLoginRejectsLockedAccountWithoutCheckingPassword(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	locked := now.Add(5 * time.Minute)
	db := &fakeDB{rows: []fakeRow{
		{vals: []any{"user-1", "$argon2id$unused", 10, locked}},
	}}
	s := &Service{DB: db, Lockout: DefaultLockoutPolicy(), Now: fixedClock(now)}

	_, err := s.Login(context.Background(), "a@b.com", "whatever")
	if !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("want ErrAccountLocked, got %v", err)
	}
	if len(db.execs) != 0 {
		t.Fatalf("locked login performed writes: %v", db.execs)
	}
}

// TestLoginWrongPasswordLocksAtThreshold proves the 10th failed attempt locks
// the account and records the deadline.
func TestLoginWrongPasswordLocksAtThreshold(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	hash, _ := HashPassword("the-real-password")
	// failed_login_count is 9; this wrong attempt makes it 10 -> lock.
	db := &fakeDB{rows: []fakeRow{
		{vals: []any{"user-1", hash, 9, nil}},
	}}
	s := &Service{DB: db, Lockout: DefaultLockoutPolicy(), Now: fixedClock(now)}

	_, err := s.Login(context.Background(), "a@b.com", "wrong")
	if !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("want ErrAccountLocked at threshold, got %v", err)
	}
	if len(db.execs) != 1 {
		t.Fatalf("want exactly one write (the lock), got %v", db.execs)
	}
}

// TestLoginWrongPasswordBelowThreshold proves an early wrong attempt returns
// ErrInvalidCredentials and increments the counter without locking.
func TestLoginWrongPasswordBelowThreshold(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	hash, _ := HashPassword("the-real-password")
	db := &fakeDB{rows: []fakeRow{
		{vals: []any{"user-1", hash, 1, nil}},
	}}
	s := &Service{DB: db, Lockout: DefaultLockoutPolicy(), Now: fixedClock(now)}

	_, err := s.Login(context.Background(), "a@b.com", "wrong")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
	if len(db.execs) != 1 {
		t.Fatalf("want one counter update, got %v", db.execs)
	}
}

// TestLoginUnknownEmailIsInvalidCredentials proves an unknown user does not
// disclose existence (returns ErrInvalidCredentials, not a distinct error).
func TestLoginUnknownEmailIsInvalidCredentials(t *testing.T) {
	db := &fakeDB{rows: []fakeRow{{err: pgx.ErrNoRows}}}
	s := &Service{DB: db, Lockout: DefaultLockoutPolicy(), Now: fixedClock(time.Now())}
	_, err := s.Login(context.Background(), "nobody@b.com", "x")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials for unknown email, got %v", err)
	}
}

// TestSignupRejectsWeakPassword proves the length floor is enforced before any
// DB call.
func TestSignupRejectsWeakPassword(t *testing.T) {
	db := &fakeDB{}
	s := NewService(db)
	if _, err := s.Signup(context.Background(), "a@b.com", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("want ErrWeakPassword, got %v", err)
	}
	if len(db.execs) != 0 {
		t.Fatal("weak-password signup touched the database")
	}
}

// TestRevokeEmptyTokenIsNoop proves logout with no cookie is a silent success.
func TestRevokeEmptyTokenIsNoop(t *testing.T) {
	db := &fakeDB{}
	s := NewService(db)
	if err := s.Revoke(context.Background(), ""); err != nil {
		t.Fatalf("Revoke(empty): %v", err)
	}
	if len(db.execs) != 0 {
		t.Fatal("empty-token revoke issued a write")
	}
}
