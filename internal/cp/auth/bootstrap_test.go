package auth

import (
	"context"
	"errors"
	"testing"
)

// bootstrapDB is a QueryRow-only DB fake that also records the upsert's SQL and
// args, so tests can assert the hash/flag actually written without loosening
// the shared fakeDB (whose QueryRow deliberately ignores args for other tests).
type bootstrapDB struct {
	inserted bool // xmax = 0 the fake pretends postgres computed
	rowErr   error
	gotSQL   string
	gotArgs  []any
	execed   bool
}

func (b *bootstrapDB) QueryRow(_ context.Context, sql string, args ...any) Row {
	b.gotSQL = sql
	b.gotArgs = args
	if b.rowErr != nil {
		return fakeRow{err: b.rowErr}
	}
	return fakeRow{vals: []any{b.inserted}}
}

func (b *bootstrapDB) Exec(_ context.Context, _ string, _ ...any) (pgconnTag, error) {
	b.execed = true
	return fakeTag{n: 1}, nil
}

// TestBootstrapAdminCreatesNewUser proves a fresh email is created as a
// platform admin: created=true, the stored hash verifies the given password,
// and is_platform_admin is the flag passed in.
func TestBootstrapAdminCreatesNewUser(t *testing.T) {
	db := &bootstrapDB{inserted: true}
	s := NewService(db)

	created, err := s.BootstrapAdmin(context.Background(), "Admin@Example.com", "correct-horse-battery", true)
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	if !created {
		t.Fatal("want created=true for a new email")
	}
	if len(db.gotArgs) != 3 {
		t.Fatalf("want 3 args (email, hash, platform_admin), got %v", db.gotArgs)
	}
	if got := db.gotArgs[0].(string); got != "admin@example.com" {
		t.Fatalf("email not normalised: got %q", got)
	}
	hash, ok := db.gotArgs[1].(string)
	if !ok {
		t.Fatalf("2nd arg not a string hash: %v", db.gotArgs[1])
	}
	if ok, err := VerifyPassword("correct-horse-battery", hash); err != nil || !ok {
		t.Fatalf("stored hash does not verify the given password: ok=%v err=%v", ok, err)
	}
	if platAdmin, ok := db.gotArgs[2].(bool); !ok || !platAdmin {
		t.Fatalf("want is_platform_admin=true written, got %v", db.gotArgs[2])
	}
}

// TestBootstrapAdminPromotesExistingUser proves an existing email is promoted
// (created=false) and that the write never carries a new password — the
// on-conflict clause in the SQL must only touch is_platform_admin.
func TestBootstrapAdminPromotesExistingUser(t *testing.T) {
	db := &bootstrapDB{inserted: false}
	s := NewService(db)

	created, err := s.BootstrapAdmin(context.Background(), "existing@b.com", "correct-horse-battery", true)
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	if created {
		t.Fatal("want created=false for an existing email (promoted, not created)")
	}
	if !containsPasswordHashOnConflictSkip(db.gotSQL) {
		t.Fatalf("on-conflict clause must not touch password_hash: %q", db.gotSQL)
	}
}

// containsPasswordHashOnConflictSkip is a crude but effective guard: the
// on-conflict SET list must mention is_platform_admin and must not mention
// password_hash (which would silently overwrite an existing password).
func containsPasswordHashOnConflictSkip(sql string) bool {
	onConflictIdx := indexOf(sql, "on conflict")
	if onConflictIdx < 0 {
		return false
	}
	tail := sql[onConflictIdx:]
	return indexOf(tail, "is_platform_admin") >= 0 && indexOf(tail, "password_hash") < 0
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// TestBootstrapAdminRevokesPlatformAdmin proves platformAdmin=false is written
// through unchanged (the `--no-platform-admin` revoke path), not silently
// coerced to true.
func TestBootstrapAdminRevokesPlatformAdmin(t *testing.T) {
	db := &bootstrapDB{inserted: false}
	s := NewService(db)

	_, err := s.BootstrapAdmin(context.Background(), "existing@b.com", "correct-horse-battery", false)
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	if len(db.gotArgs) != 3 {
		t.Fatalf("want 3 args (email, hash, platform_admin), got %v", db.gotArgs)
	}
	if platAdmin, ok := db.gotArgs[2].(bool); !ok || platAdmin {
		t.Fatalf("want is_platform_admin=false written, got %v", db.gotArgs[2])
	}
}

// TestBootstrapAdminRejectsShortPasswordBeforeWrite proves the minPasswordLen
// floor is enforced before any DB call, exactly as Signup does.
func TestBootstrapAdminRejectsShortPasswordBeforeWrite(t *testing.T) {
	db := &bootstrapDB{}
	s := NewService(db)

	_, err := s.BootstrapAdmin(context.Background(), "a@b.com", "short", true)
	if !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("want ErrWeakPassword, got %v", err)
	}
	if db.gotSQL != "" || db.execed {
		t.Fatal("short-password bootstrap touched the database")
	}
}

// TestBootstrapAdminRejectsEmptyEmail proves an empty (post-normalisation)
// email is rejected before any DB call.
func TestBootstrapAdminRejectsEmptyEmail(t *testing.T) {
	db := &bootstrapDB{}
	s := NewService(db)

	_, err := s.BootstrapAdmin(context.Background(), "   ", "correct-horse-battery", true)
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials for empty email, got %v", err)
	}
	if db.gotSQL != "" || db.execed {
		t.Fatal("empty-email bootstrap touched the database")
	}
}
