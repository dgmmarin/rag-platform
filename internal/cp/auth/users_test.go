package auth

import (
	"context"
	"errors"
	"testing"
)

// UserByEmail returns the control-plane user id for an existing email so
// AddMember can resolve an admin-typed email to a user (STORY-11.5, ISSUE-0064).
func TestUserByEmailFound(t *testing.T) {
	svc := &MembershipService{DB: &fakeDB{rows: []fakeRow{{vals: []any{"user-123"}}}}}
	id, err := svc.UserByEmail(context.Background(), "a@b.com")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	if id != "user-123" {
		t.Fatalf("id = %q, want user-123", id)
	}
}

// A missing email maps to ErrUserNotFound (not a generic error) so the handler
// can answer 404 "must sign up first" rather than 500.
func TestUserByEmailNotFound(t *testing.T) {
	svc := &MembershipService{DB: &fakeDB{rows: []fakeRow{{err: errNoRows{}}}}}
	_, err := svc.UserByEmail(context.Background(), "ghost@b.com")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("got %v, want ErrUserNotFound", err)
	}
}
