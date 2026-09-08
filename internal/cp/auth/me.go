package auth

import "context"

// MeUser is the minimal user identity the admin UI hydrates on load.
type MeUser struct {
	ID, Email string
}

// MeMembership is one tenant membership row for the admin UI's tenant switcher.
type MeMembership struct {
	TenantID, Slug, Name string
	Role                 Role
}

// MeView is everything GET /v1/auth/me returns: the caller's identity, their
// platform-admin flag, and their tenant memberships (SPEC-11 §2.1).
type MeView struct {
	User            MeUser
	IsPlatformAdmin bool
	Memberships     []MeMembership
}

// MeService resolves the current user + their tenant memberships for the admin
// UI (SPEC-11 §2.1). Control-plane registry/auth data only — no tenant content.
type MeService struct {
	DB MembershipDB
}

// NewMeService builds a MeService over the given DB.
func NewMeService(db MembershipDB) *MeService {
	return &MeService{DB: db}
}

// Me loads the user's identity/admin flag and their tenant memberships.
func (s *MeService) Me(ctx context.Context, userID string) (MeView, error) {
	var v MeView
	v.User.ID = userID
	if err := s.DB.QueryRow(ctx,
		`select email, is_platform_admin from users where id = $1`, userID,
	).Scan(&v.User.Email, &v.IsPlatformAdmin); err != nil {
		return MeView{}, err
	}

	rows, err := s.DB.Query(ctx, `
		select t.id::text, t.slug, t.name, m.role
		from tenant_members m join tenants t on t.id = m.tenant_id
		where m.user_id = $1 order by t.name`, userID)
	if err != nil {
		return MeView{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MeMembership
		var role string
		if err := rows.Scan(&m.TenantID, &m.Slug, &m.Name, &role); err != nil {
			return MeView{}, err
		}
		m.Role = Role(role)
		v.Memberships = append(v.Memberships, m)
	}
	return v, rows.Err()
}
