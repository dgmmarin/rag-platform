package auth

import (
	"context"
	"errors"
	"fmt"
)

// Membership sentinel errors let HTTP handlers map failures to status codes
// without string matching.
var (
	// ErrAlreadyMember is returned when adding a user that is already a member of
	// the tenant.
	ErrAlreadyMember = errors.New("auth: user is already a member")
	// ErrNotMember is returned when updating/removing a membership that does not
	// exist for the (tenant, user).
	ErrNotMember = errors.New("auth: user is not a member")
	// ErrLastOwner is returned when an operation would leave a tenant with no
	// owner: removing the sole owner, or demoting the sole owner (FR-ACC-06,
	// SPEC-02 §4). The invariant is enforced atomically at the SQL layer.
	ErrLastOwner = errors.New("auth: cannot remove or demote the last owner")
)

// Member is one row of tenant_members joined to the user's email for display.
type Member struct {
	TenantID string
	UserID   string
	Email    string
	Role     Role
}

// Rows is the minimal multi-row surface ListMembers scans; pgx.Rows satisfies it.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// MembershipDB is the database surface the membership service needs. It extends
// the shared Exec/QueryRow surface with Query for the list read. *pgxpool.Pool
// satisfies it via poolDB.
type MembershipDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconnTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// MembershipService owns tenant_members (SPEC-02 §2). It is stateless and safe
// for concurrent use. The last-owner invariant (SPEC-02 §4) is enforced in a
// single guarded SQL statement per mutation, so it holds even under concurrent
// removals without a read-modify-write race.
type MembershipService struct {
	DB MembershipDB
}

// NewMembershipService builds a MembershipService over the given DB.
func NewMembershipService(db MembershipDB) *MembershipService {
	return &MembershipService{DB: db}
}

// AddMember adds a user to a tenant with the given role (FR-ACC-02). A duplicate
// (tenant, user) is rejected with ErrAlreadyMember; an invalid role is rejected
// before any write.
func (s *MembershipService) AddMember(ctx context.Context, tenantID, userID string, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("%w: %q", errors.New("auth: invalid role"), role)
	}
	var ok bool
	err := s.DB.QueryRow(ctx,
		`insert into tenant_members (tenant_id, user_id, role) values ($1, $2, $3::tenant_role) returning true`,
		tenantID, userID, string(role)).Scan(&ok)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrAlreadyMember
		}
		return fmt.Errorf("auth: add member: %w", err)
	}
	return nil
}

// ListMembers returns every membership of a tenant with the member's email,
// ordered by email for a stable listing (FR-ACC-02).
func (s *MembershipService) ListMembers(ctx context.Context, tenantID string) ([]Member, error) {
	rows, err := s.DB.Query(ctx,
		`select tm.tenant_id::text, tm.user_id::text, u.email::text, tm.role::text
		 from tenant_members tm join users u on u.id = tm.user_id
		 where tm.tenant_id = $1
		 order by u.email`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("auth: list members: %w", err)
	}
	defer rows.Close()

	var out []Member
	for rows.Next() {
		var m Member
		var role string
		if err := rows.Scan(&m.TenantID, &m.UserID, &m.Email, &role); err != nil {
			return nil, fmt.Errorf("auth: scan member: %w", err)
		}
		m.Role = Role(role)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: iterate members: %w", err)
	}
	return out, nil
}

// SetMemberRole changes an existing member's role (FR-ACC-02/06). The update is
// guarded so it never demotes the sole owner: the WHERE clause refuses to change
// an owner row away from 'owner' while it is the only owner. A zero-row result is
// disambiguated by whether the member exists (ErrNotMember) or the guard blocked
// it (ErrLastOwner).
func (s *MembershipService) SetMemberRole(ctx context.Context, tenantID, userID string, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("%w: %q", errors.New("auth: invalid role"), role)
	}
	tag, err := s.DB.Exec(ctx,
		`update tenant_members set role = $3::tenant_role
		 where tenant_id = $1 and user_id = $2
		   and not (
		     role = 'owner' and $3::tenant_role <> 'owner'
		     and (select count(*) from tenant_members where tenant_id = $1 and role = 'owner') <= 1
		   )`,
		tenantID, userID, string(role))
	if err != nil {
		return fmt.Errorf("auth: set member role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.disambiguateZero(ctx, tenantID, userID)
	}
	return nil
}

// RemoveMember removes a user from a tenant (FR-ACC-02). The delete is guarded so
// it never removes the sole owner (SPEC-02 §4 invariant). A zero-row result is
// disambiguated into ErrNotMember or ErrLastOwner.
func (s *MembershipService) RemoveMember(ctx context.Context, tenantID, userID string) error {
	tag, err := s.DB.Exec(ctx,
		`delete from tenant_members
		 where tenant_id = $1 and user_id = $2
		   and not (
		     role = 'owner'
		     and (select count(*) from tenant_members where tenant_id = $1 and role = 'owner') <= 1
		   )`,
		tenantID, userID)
	if err != nil {
		return fmt.Errorf("auth: remove member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return s.disambiguateZero(ctx, tenantID, userID)
	}
	return nil
}

// disambiguateZero decides why a guarded mutation affected no rows: the member
// does not exist (ErrNotMember) or the last-owner guard blocked it (ErrLastOwner).
func (s *MembershipService) disambiguateZero(ctx context.Context, tenantID, userID string) error {
	var exists bool
	err := s.DB.QueryRow(ctx,
		`select exists(select 1 from tenant_members where tenant_id = $1 and user_id = $2)`,
		tenantID, userID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("auth: check membership: %w", err)
	}
	if !exists {
		return ErrNotMember
	}
	return ErrLastOwner
}
