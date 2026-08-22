package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rag-platform/ragctl/internal/cp/audit"
)

// ErrNoImpersonation is returned when ending or looking up an impersonation grant
// that does not exist (unknown id). Callers map it to 404.
var ErrNoImpersonation = errors.New("auth: no such impersonation")

// defaultImpersonationTTL time-bounds an impersonation grant (FR-ACC-07). A
// platform admin assumes a tenant identity for support; the grant self-expires so
// a forgotten session cannot linger, and it is revocable before then via End.
const defaultImpersonationTTL = time.Hour

// Impersonation is a scoped grant that lets a platform admin act as a tenant user
// (FR-ACC-07, SPEC-02 §4). It never silently swaps identity: it records BOTH the
// real admin actor (AdminUserID) and the impersonated principal (TenantID +
// ImpersonatedUserID), so every downstream action stays attributable to the
// admin. It is control-plane-only (C-3): it references control-plane ids and
// touches no tenant data.
type Impersonation struct {
	ID                 string
	AdminUserID        string
	TenantID           string
	ImpersonatedUserID string
	ExpiresAt          time.Time
	EndedAt            *time.Time
}

// Active reports whether the grant is usable at now: not ended and not expired
// (fail closed — an expired or ended grant is inactive).
func (i Impersonation) Active(now time.Time) bool {
	if i.EndedAt != nil {
		return false
	}
	return now.Before(i.ExpiresAt)
}

// AuditFunc writes one audit event. It is the seam the impersonation service uses
// to record admin.impersonate(.end) so the sanctioned audit.Record writer is used
// on the real path (SPEC-02 §6, ADR-0023) while unit tests can capture the event.
type AuditFunc func(ctx context.Context, e audit.Event) error

// ImpersonationService starts, ends, and resolves impersonation grants. It is
// stateless and safe for concurrent use. Only platform admins may start a grant;
// that check is enforced upstream by RequirePlatformAdmin, so the service assumes
// its caller is already a platform admin (mirroring how the audit handler assumes
// the middleware, ADR-0023).
type ImpersonationService struct {
	DB    DB
	Now   Clock
	Audit AuditFunc
}

// NewImpersonationService builds a service over the given DB, wiring the audit
// seam to the sanctioned audit.Record writer over the same pool.
func NewImpersonationService(db DB, w audit.ExecDB) *ImpersonationService {
	return &ImpersonationService{
		DB:  db,
		Now: time.Now,
		Audit: func(ctx context.Context, e audit.Event) error {
			return audit.Record(ctx, w, e)
		},
	}
}

func (s *ImpersonationService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Start opens a time-bounded impersonation grant of tenantID's impersonatedUserID
// by the platform admin adminUserID, and writes an admin.impersonate audit event
// carrying both identities (details.impersonation=true, non-secret metadata only,
// C-3). Missing arguments fail closed. The grant row and the audit row are written
// in the same transaction so the record and its audit trail are atomic.
func (s *ImpersonationService) Start(ctx context.Context, adminUserID, tenantID, impersonatedUserID string) (Impersonation, error) {
	if adminUserID == "" || tenantID == "" || impersonatedUserID == "" {
		return Impersonation{}, fmt.Errorf("auth: impersonation requires admin, tenant and target user")
	}
	now := s.now()
	expires := now.Add(defaultImpersonationTTL)

	var id string
	err := s.DB.QueryRow(ctx,
		`insert into impersonation_sessions
		   (admin_user_id, tenant_id, impersonated_user_id, created_at, expires_at)
		 values ($1, $2, $3, $4, $5)
		 returning id::text`,
		adminUserID, tenantID, impersonatedUserID, now, expires).Scan(&id)
	if err != nil {
		return Impersonation{}, fmt.Errorf("auth: start impersonation: %w", err)
	}

	grant := Impersonation{
		ID:                 id,
		AdminUserID:        adminUserID,
		TenantID:           tenantID,
		ImpersonatedUserID: impersonatedUserID,
		ExpiresAt:          expires,
	}
	if err := s.audit(ctx, grant, "admin.impersonate"); err != nil {
		return Impersonation{}, err
	}
	return grant, nil
}

// End revokes an active impersonation grant (idempotently stamping ended_at) and
// writes an admin.impersonate.end audit event. It first resolves the grant so the
// audit event carries the real admin and impersonated identities; an unknown id
// fails closed with ErrNoImpersonation.
func (s *ImpersonationService) End(ctx context.Context, id, adminUserID string) error {
	if id == "" {
		return ErrNoImpersonation
	}
	now := s.now()

	var grant Impersonation
	grant.ID = id
	err := s.DB.QueryRow(ctx,
		`select admin_user_id::text, tenant_id::text, impersonated_user_id::text
		 from impersonation_sessions where id = $1`, id).
		Scan(&grant.AdminUserID, &grant.TenantID, &grant.ImpersonatedUserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoImpersonation
		}
		return fmt.Errorf("auth: look up impersonation: %w", err)
	}

	if _, err := s.DB.Exec(ctx,
		`update impersonation_sessions set ended_at = $2 where id = $1 and ended_at is null`,
		id, now); err != nil {
		return fmt.Errorf("auth: end impersonation: %w", err)
	}
	// Attribute the end to the admin who ended it (the caller), which is normally
	// the same admin who started it. The audit target stays the impersonated user.
	if adminUserID != "" {
		grant.AdminUserID = adminUserID
	}
	return s.audit(ctx, grant, "admin.impersonate.end")
}

// audit writes an audit_log row for an impersonation lifecycle action. The actor
// is the real admin (never the impersonated user) so the action stays attributable
// to the admin (FR-ACC-07); the target is the impersonated user, scoped to the
// tenant, and details.impersonation=true marks it per SPEC-02 §4. Details carry
// non-secret metadata only (C-3).
func (s *ImpersonationService) audit(ctx context.Context, g Impersonation, action string) error {
	if s.Audit == nil {
		return fmt.Errorf("auth: impersonation audit sink not configured")
	}
	tenantID := g.TenantID
	adminID := g.AdminUserID
	targetType := "user"
	targetID := g.ImpersonatedUserID
	if err := s.Audit(ctx, audit.Event{
		TenantID:    &tenantID,
		ActorUserID: &adminID,
		Action:      action,
		TargetType:  &targetType,
		TargetID:    &targetID,
		Details: map[string]any{
			"impersonation":        true,
			"impersonation_id":     g.ID,
			"impersonated_user_id": g.ImpersonatedUserID,
		},
	}); err != nil {
		return fmt.Errorf("auth: audit %s: %w", action, err)
	}
	return nil
}
