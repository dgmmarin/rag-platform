package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/cp/audit"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// RequireTenantAccess returns middleware that authorises a session request
// against the tenant named by the {tenantId} path segment (STORY-11.2,
// ADR-0075). It must run inside RequireSession. Unlike RequireRole (which
// authorises against a tenant already resolved onto the context — the API-key
// surface, tenant derived from the credential per FR-ACC-03), this resolves the
// tenant FROM the path, because the session admin UI lets a platform admin act
// on a tenant chosen in the UI rather than named by a credential.
//
// Responses:
//   - 401: no session.
//   - 404: an unparseable tenant id, OR a session user who is neither a member
//     of that tenant nor a platform admin. Both cases return the same 404 so the
//     surface never leaks whether a tenant id exists to someone with no claim on
//     it (ADR-0075).
//   - 403: a member whose role does not grant perm (SPEC-02 §4 matrix, roles.go).
//   - otherwise: the inner handler runs with tenant.WithTenantID set to the
//     resolved id, the same key the reused sources.Handlers already read via
//     tenant.TenantIDFromCtx.
//
// A platform admin acting on a tenant they are NOT a member of is audited with
// details.impersonation=true (SPEC-02 §4, FR-ADM-05) through the Audit seam —
// the same AuditFunc shape ImpersonationService.Start writes through. A
// platform admin who also holds tenant membership is not "acting across
// tenants" in the FR-ADM-05 sense, so that path audits nothing extra here (it
// still passes the role check like any other member, only skipped by virtue of
// being a platform admin).
func (a *AuthzService) RequireTenantAccess(perm Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := SessionFrom(r.Context())
			if !ok || sess.UserID == "" {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}

			rawID, err := uuid.Parse(r.PathValue("tenantId"))
			if err != nil {
				writeError(w, http.StatusNotFound, "resource not found")
				return
			}
			tid := tenant.ID(rawID)

			p, err := a.lookupPrincipal(r.Context(), tid.String(), sess.UserID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "authorization failed")
				return
			}
			if !p.member && !p.platformAdmin {
				writeError(w, http.StatusNotFound, "resource not found")
				return
			}
			if p.platformAdmin && !p.member {
				if err := a.auditTenantAccess(r.Context(), tid.String(), sess.UserID); err != nil {
					writeError(w, http.StatusInternalServerError, "authorization failed")
					return
				}
			} else if !p.platformAdmin && !p.role.Can(perm) {
				writeError(w, http.StatusForbidden, "insufficient permissions")
				return
			}

			ctx := tenant.WithTenantID(r.Context(), tid)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// auditTenantAccess records a platform admin's cross-tenant session access
// (SPEC-02 §4, FR-ADM-05) — the same details.impersonation=true shape
// ImpersonationService.audit writes for an explicit impersonation grant, but
// for the ambient "acting on this tenant's session-scoped admin API" path.
func (a *AuthzService) auditTenantAccess(ctx context.Context, tenantID, adminUserID string) error {
	if a.Audit == nil {
		return fmt.Errorf("auth: tenant-access audit sink not configured")
	}
	if err := a.Audit(ctx, audit.Event{
		TenantID:    &tenantID,
		ActorUserID: &adminUserID,
		Action:      "admin.tenant_access",
		Details:     map[string]any{"impersonation": true},
	}); err != nil {
		return fmt.Errorf("auth: audit admin.tenant_access: %w", err)
	}
	return nil
}
