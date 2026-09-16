package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrUserNotFound is returned by UserByEmail when no user has the given email,
// letting the members handler answer 404 "they must sign up first" rather than a
// generic 500 (STORY-11.5, ISSUE-0064). This story has no invite/email flow:
// AddMember can only add an already-registered user.
var ErrUserNotFound = errors.New("auth: no user with that email")

// UserByEmail resolves a control-plane user id from an email so an admin can add
// a member by the email they know rather than an opaque id (FR-ACC-02). It reads
// only the control-plane users table (C-3). A missing row is ErrUserNotFound.
func (s *MembershipService) UserByEmail(ctx context.Context, email string) (string, error) {
	var id string
	err := s.DB.QueryRow(ctx, `select id::text from users where email = $1`, email).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrUserNotFound
		}
		return "", fmt.Errorf("auth: user by email: %w", err)
	}
	return id, nil
}
