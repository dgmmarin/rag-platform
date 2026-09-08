package auth

import (
	"context"
	"fmt"
)

// BootstrapAdmin creates a user (with an argon2id password hash) or, if the
// email already exists, promotes it — in one race-safe `insert ... on conflict`
// write (STORY-11.1 Task 6, FR-ADM-07, ADR-0074). It never touches the password
// of an existing user: on conflict only is_platform_admin is updated, so a
// re-run against an already-bootstrapped email cannot silently change its
// password. A platform-admin user needs no tenant_members row: is_platform_admin
// alone grants the cross-tenant admin surface (SPEC-02 §4).
//
// It reuses Signup's validation (normalizeEmail, minPasswordLen) and hashing
// (HashPassword) so the two paths cannot drift, and rejects a bad email/password
// before any write. created reports whether the row was freshly inserted
// (true) or an existing user was promoted (false).
func (s *Service) BootstrapAdmin(ctx context.Context, email, password string, platformAdmin bool) (bool, error) {
	email = normalizeEmail(email)
	if email == "" {
		return false, fmt.Errorf("%w: email required", ErrInvalidCredentials)
	}
	if len(password) < minPasswordLen {
		return false, fmt.Errorf("%w: at least %d characters", ErrWeakPassword, minPasswordLen)
	}
	hash, err := HashPassword(password)
	if err != nil {
		return false, err
	}

	// xmax = 0 on the returned row means the row was just inserted (mirrors the
	// eval-case upsert, internal/eval/store.go); anything else means the on-conflict
	// branch ran instead, i.e. an existing user was promoted.
	var created bool
	err = s.DB.QueryRow(ctx, `
		insert into users (email, password_hash, is_platform_admin)
		values ($1, $2, $3)
		on conflict (email) do update
		set is_platform_admin = excluded.is_platform_admin
		returning (xmax = 0)`,
		email, hash, platformAdmin).Scan(&created)
	if err != nil {
		return false, fmt.Errorf("auth: bootstrap admin: %w", err)
	}
	return created, nil
}
