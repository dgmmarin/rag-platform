package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// AuthzService enforces the SPEC-02 §4 role matrix as HTTP middleware. It reads
// the authenticated user from the session context (STORY-03.1) and the resolved
// tenant from the tenant context (SPEC-01, FR-ACC-03) — never from a
// client-supplied parameter — looks up that user's role in that tenant, and
// checks it against the permission the route requires.
//
// STORY-04.1 attaches RequireRole to concrete routes; this story implements the
// middleware and the matrix so 04.1 only wires it.
type AuthzService struct {
	// DB exposes the role lookup. Only QueryRow is used; the wider MembershipDB
	// interface is accepted so a single *pgxpool.Pool wrapper serves both the
	// membership service and this middleware.
	DB interface {
		QueryRow(ctx context.Context, sql string, args ...any) Row
	}
}

// NewAuthzService builds an AuthzService over the given DB.
func NewAuthzService(db MembershipDB) *AuthzService {
	return &AuthzService{DB: db}
}

// principal is the resolved (role, isPlatformAdmin) for a user in a tenant.
type principal struct {
	role          Role
	platformAdmin bool
	member        bool
}

// lookupPrincipal resolves the user's role in the tenant and whether the user is
// a platform admin, in one query. A user may be a platform admin without being a
// member (FR-ACC-07); such a user is returned with member=false.
func (a *AuthzService) lookupPrincipal(ctx context.Context, tenantID, userID string) (principal, error) {
	var role *string
	var plat bool
	err := a.DB.QueryRow(ctx,
		`select tm.role::text, u.is_platform_admin
		 from users u
		 left join tenant_members tm on tm.user_id = u.id and tm.tenant_id = $1
		 where u.id = $2`,
		tenantID, userID).Scan(&role, &plat)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return principal{}, nil
		}
		return principal{}, fmt.Errorf("auth: look up principal: %w", err)
	}
	p := principal{platformAdmin: plat}
	if role != nil {
		p.role = Role(*role)
		p.member = true
	}
	return p, nil
}

// RequirePlatformAdmin returns middleware that admits only platform admins
// (FR-ACC-07). Unlike RequireRole it is tenant-less: platform-admin endpoints
// (e.g. GET /admin/audit?tenant=) act across tenants, naming the tenant as a
// parameter rather than resolving it from the credential. It must run inside
// RequireSession. Responses: 401 with no session; 403 when the user is not a
// platform admin; otherwise the inner handler runs.
func (a *AuthzService) RequirePlatformAdmin() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := SessionFrom(r.Context())
			if !ok || sess.UserID == "" {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			var plat bool
			err := a.DB.QueryRow(r.Context(),
				`select is_platform_admin from users where id = $1`, sess.UserID).Scan(&plat)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					writeError(w, http.StatusForbidden, "platform admin required")
					return
				}
				writeError(w, http.StatusInternalServerError, "authorization failed")
				return
			}
			if !plat {
				writeError(w, http.StatusForbidden, "platform admin required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole returns middleware that authorises the request for the given
// permission. It must run inside RequireSession (it reads the session) and after
// tenant resolution (it reads the tenant ID). Responses: 401 when there is no
// session or no resolved tenant; 403 when the user is not a member or the member's
// role does not grant the permission; otherwise the inner handler runs.
func (a *AuthzService) RequireRole(perm Permission) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := SessionFrom(r.Context())
			if !ok || sess.UserID == "" {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			tid, ok := tenant.TenantIDFromCtx(r.Context())
			if !ok {
				// No tenant resolved: nothing to authorise against (FR-ACC-03).
				writeError(w, http.StatusUnauthorized, "no tenant resolved")
				return
			}

			p, err := a.lookupPrincipal(r.Context(), tid.String(), sess.UserID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "authorization failed")
				return
			}

			// Platform admins may act on any tenant (FR-ACC-07, SPEC-02 §4). The
			// impersonation audit event is written by the acting handler.
			if p.platformAdmin {
				next.ServeHTTP(w, r)
				return
			}
			if !p.member || !p.role.Can(perm) {
				writeError(w, http.StatusForbidden, "insufficient permissions")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
