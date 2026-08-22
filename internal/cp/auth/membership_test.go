package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// memMembers is an in-memory tenant_members store implementing the auth DB
// interface for the statements the membership service issues. It lets the branch
// logic (validation, the last-owner invariant, not-found handling) be unit-tested
// without Postgres; the real SQL round trip is proven in the e2e.
type memMembers struct {
	// rows keyed by tenant_id|user_id -> role.
	rows map[string]Role
}

func newMemMembers() *memMembers { return &memMembers{rows: map[string]Role{}} }

func key(tenant, user string) string { return tenant + "|" + user }

func (m *memMembers) ownerCount(tenant string) int {
	n := 0
	for k, r := range m.rows {
		if strings.HasPrefix(k, tenant+"|") && r == RoleOwner {
			n++
		}
	}
	return n
}

func (m *memMembers) Exec(_ context.Context, sql string, args ...any) (pgconnTag, error) {
	switch {
	// SetMemberRole: guarded so it never demotes the last owner.
	case strings.Contains(sql, "update tenant_members set role"):
		tenant, user, newRole := args[0].(string), args[1].(string), args[2].(string)
		cur, ok := m.rows[key(tenant, user)]
		if !ok {
			return fakeTag{n: 0}, nil
		}
		if cur == RoleOwner && Role(newRole) != RoleOwner && m.ownerCount(tenant) <= 1 {
			return fakeTag{n: 0}, nil // guard blocks the demotion
		}
		m.rows[key(tenant, user)] = Role(newRole)
		return fakeTag{n: 1}, nil
	// RemoveMember: guarded so it never removes the last owner.
	case strings.Contains(sql, "delete from tenant_members"):
		tenant, user := args[0].(string), args[1].(string)
		cur, ok := m.rows[key(tenant, user)]
		if !ok {
			return fakeTag{n: 0}, nil
		}
		if cur == RoleOwner && m.ownerCount(tenant) <= 1 {
			return fakeTag{n: 0}, nil // guard blocks removing the last owner
		}
		delete(m.rows, key(tenant, user))
		return fakeTag{n: 1}, nil
	}
	return fakeTag{n: 0}, nil
}

func (m *memMembers) QueryRow(_ context.Context, sql string, args ...any) Row {
	switch {
	case strings.Contains(sql, "insert into tenant_members"):
		tenant, user, role := args[0].(string), args[1].(string), args[2].(string)
		if _, exists := m.rows[key(tenant, user)]; exists {
			return fakeRow{err: uniqueErr{}}
		}
		m.rows[key(tenant, user)] = Role(role)
		return fakeRow{vals: []any{true}}
	case strings.Contains(sql, "select exists"):
		tenant, user := args[0].(string), args[1].(string)
		_, exists := m.rows[key(tenant, user)]
		return fakeRow{vals: []any{exists}}
	}
	return fakeRow{err: errNoRows{}}
}

// Query is unused by the unit tests (ListMembers is exercised in the e2e against
// real Postgres); it exists only to satisfy MembershipDB.
func (m *memMembers) Query(_ context.Context, _ string, _ ...any) (Rows, error) {
	return nil, errNoRows{}
}

func newMembershipService() (*MembershipService, *memMembers) {
	store := newMemMembers()
	return &MembershipService{DB: store}, store
}

func TestAddMemberRejectsInvalidRole(t *testing.T) {
	svc, _ := newMembershipService()
	err := svc.AddMember(context.Background(), "t1", "u1", Role("superadmin"))
	if err == nil {
		t.Fatal("AddMember accepted an invalid role")
	}
}

func TestAddMemberRejectsDuplicate(t *testing.T) {
	svc, _ := newMembershipService()
	ctx := context.Background()
	if err := svc.AddMember(ctx, "t1", "u1", RoleEditor); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := svc.AddMember(ctx, "t1", "u1", RoleAdmin); !errors.Is(err, ErrAlreadyMember) {
		t.Fatalf("duplicate add: got %v, want ErrAlreadyMember", err)
	}
}

func TestSetMemberRoleUpdates(t *testing.T) {
	svc, store := newMembershipService()
	ctx := context.Background()
	_ = svc.AddMember(ctx, "t1", "u1", RoleViewer)
	if err := svc.SetMemberRole(ctx, "t1", "u1", RoleEditor); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if store.rows[key("t1", "u1")] != RoleEditor {
		t.Fatalf("role = %q, want editor", store.rows[key("t1", "u1")])
	}
}

func TestSetMemberRoleUnknownMember(t *testing.T) {
	svc, _ := newMembershipService()
	if err := svc.SetMemberRole(context.Background(), "t1", "ghost", RoleEditor); !errors.Is(err, ErrNotMember) {
		t.Fatalf("got %v, want ErrNotMember", err)
	}
}

func TestRemoveMember(t *testing.T) {
	svc, store := newMembershipService()
	ctx := context.Background()
	_ = svc.AddMember(ctx, "t1", "owner1", RoleOwner)
	_ = svc.AddMember(ctx, "t1", "ed1", RoleEditor)
	if err := svc.RemoveMember(ctx, "t1", "ed1"); err != nil {
		t.Fatalf("remove editor: %v", err)
	}
	if _, ok := store.rows[key("t1", "ed1")]; ok {
		t.Fatal("member not removed")
	}
}

// TestRemoveLastOwnerRejected is the core invariant: removing the sole owner is
// refused so a tenant is never left ownerless.
func TestRemoveLastOwnerRejected(t *testing.T) {
	svc, store := newMembershipService()
	ctx := context.Background()
	_ = svc.AddMember(ctx, "t1", "owner1", RoleOwner)
	if err := svc.RemoveMember(ctx, "t1", "owner1"); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("remove last owner: got %v, want ErrLastOwner", err)
	}
	if _, ok := store.rows[key("t1", "owner1")]; !ok {
		t.Fatal("last owner was removed despite the invariant")
	}
}

// TestRemoveOwnerAllowedWhenAnotherExists proves the invariant only blocks the
// LAST owner: with two owners, removing one succeeds.
func TestRemoveOwnerAllowedWhenAnotherExists(t *testing.T) {
	svc, _ := newMembershipService()
	ctx := context.Background()
	_ = svc.AddMember(ctx, "t1", "owner1", RoleOwner)
	_ = svc.AddMember(ctx, "t1", "owner2", RoleOwner)
	if err := svc.RemoveMember(ctx, "t1", "owner1"); err != nil {
		t.Fatalf("remove one of two owners: %v", err)
	}
}

// TestDemoteLastOwnerRejected proves the invariant also blocks demoting the sole
// owner via a role change (an owner can't strip its own ownership if it's alone).
func TestDemoteLastOwnerRejected(t *testing.T) {
	svc, _ := newMembershipService()
	ctx := context.Background()
	_ = svc.AddMember(ctx, "t1", "owner1", RoleOwner)
	if err := svc.SetMemberRole(ctx, "t1", "owner1", RoleAdmin); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demote last owner: got %v, want ErrLastOwner", err)
	}
}
