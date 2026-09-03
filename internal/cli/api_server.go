package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/config"
	"github.com/rag-platform/ragctl/internal/cp/audit"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/ratelimit"
	"github.com/rag-platform/ragctl/internal/cp/sources"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/cp/usage"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/obs"
)

// apiServer holds the assembled public router plus the background loops (the
// rate-limiter idle sweep and the usage-counter flush) `serve` must run and stop
// alongside the HTTP server (STORY-04.1, SPEC-07 §1).
type apiServer struct {
	Handler http.Handler
	pool    *pgxpool.Pool
	limiter *ratelimit.Limiter
	usage   *usage.Counter
}

// buildAPIServer wires the control-plane services (EPIC-03) into the SPEC-07 §1
// router. It opens a single control-plane pool (never a tenant pool — C-3), then
// constructs every middleware and handler from it: password + OIDC auth, the
// platform-admin surface (audit read, impersonation), the tenant-scoped surface
// (usage) behind API-key scope + rate limiting.
//
// It fails closed on a missing control-plane URL. OIDC is wired only when
// configured (an empty issuer leaves the OIDC routes as not-implemented seams).
func buildAPIServer(ctx context.Context, log *slog.Logger, metrics *obs.Metrics, cfg config.Config, controlURL string, cipher *crypto.Cipher, secure bool) (*apiServer, error) {
	if controlURL == "" {
		return nil, fmt.Errorf("serve: no control-plane URL (set --control-plane-url or CONTROL_PLANE_URL)")
	}
	_ = cipher // reserved for tenant-plane resolution wired by later EPIC-04 routes.

	pool, err := pgxpool.New(ctx, controlURL)
	if err != nil {
		return nil, fmt.Errorf("serve: open control-plane pool: %w", err)
	}

	// --- Auth (password + sessions). ---
	authSvc := auth.NewService(auth.FromPool(pool))
	authHandlers := &auth.Handlers{Service: authSvc, Secure: secure}
	membershipDB := auth.MembershipFromPool(pool)
	authz := auth.NewAuthzService(membershipDB)
	verifier := auth.NewAPIKeyVerifier(auth.FromPool(pool))

	// --- OIDC (optional). ---
	var oidcStart, oidcCallback http.Handler
	oidcCfg := auth.OIDCConfig{
		Issuer:          cfg.OIDCIssuer,
		ClientID:        cfg.OIDCClientID,
		ClientSecret:    cfg.OIDCClientSecret,
		RedirectURL:     cfg.OIDCRedirectURL,
		JITProvisioning: cfg.OIDCJITProvisioning,
	}
	if oidcCfg.Enabled() {
		oidcSvc, oerr := auth.NewOIDCProvider(ctx, oidcCfg)
		if oerr != nil {
			pool.Close()
			return nil, fmt.Errorf("serve: oidc: %w", oerr)
		}
		oidcSvc.Auth = authSvc
		oidcHandlers := &auth.OIDCHandlers{Service: oidcSvc, Secure: secure}
		oidcStart = http.HandlerFunc(oidcHandlers.Start)
		oidcCallback = http.HandlerFunc(oidcHandlers.Callback)
	}

	// --- Platform-admin handlers. ---
	auditHandlers := audit.NewHandlers(audit.NewService(audit.FromPool(pool)))
	impSvc := auth.NewImpersonationService(auth.FromPool(pool), audit.FromPool(pool))
	impHandlers := auth.NewImpersonationHandlers(impSvc)

	// --- Usage (tenant-scoped read + background flush). ---
	usageCounter := usage.NewCounter(usage.FromPool(pool))
	usageHandlers := usage.NewHandlers(usage.NewService(usage.FromPool(pool)))

	// --- Sources (tenant-scoped CRUD + sync/delete enqueue, STORY-04.3). The
	// connector-framework Validator is left nil until EPIC-06 (STORY-06.1) wires
	// the registry: create/update run generic validation and /test reports the
	// seam envelope. Sources are control-plane registry data (C-3), so this uses
	// the control-plane pool — never a tenant pool. ---
	sourceHandlers := sources.NewHandlers(sources.NewService(sources.FromPool(pool)))

	// --- Rate limiting (per key + per tenant, credential-keyed). ---
	limiter := ratelimit.New(nil)
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	rl := &ratelimit.Middleware{
		Limiter:     limiter,
		Limit:       ratelimit.LimitFromSettings(settingsSvc, cfg.RateLimitDefaultQPS),
		Burst:       cfg.RateLimitKeyBurst,
		TenantBurst: cfg.RateLimitTenantBurst,
	}

	deps := api.Deps{
		Log:     log,
		Metrics: metrics,
		Checks:  []obs.Check{controlPlaneCheck(pool)},

		RequireSession:       authHandlers.RequireSession,
		CSRF:                 authHandlers.CSRF,
		RequirePlatformAdmin: authz.RequirePlatformAdmin(),
		RequireScopeQuery:    verifier.RequireScope(auth.ScopeQuery),
		RequireScopeIngest:   verifier.RequireScope(auth.ScopeIngest),
		RequireScopeAdmin:    verifier.RequireScope(auth.ScopeAdmin),
		RequireRoleAdmin:     authz.RequireRole(auth.PermManageMembers),
		RateLimit:            rl.Handler,

		Signup:             http.HandlerFunc(authHandlers.Signup),
		Login:              http.HandlerFunc(authHandlers.Login),
		Logout:             http.HandlerFunc(authHandlers.Logout),
		OIDCStart:          oidcStart,
		OIDCCallback:       oidcCallback,
		AuditList:          http.HandlerFunc(auditHandlers.List),
		UsageList:          http.HandlerFunc(usageHandlers.List),
		ImpersonationStart: http.HandlerFunc(impHandlers.Start),
		ImpersonationEnd:   http.HandlerFunc(impHandlers.End),

		SourceList:   http.HandlerFunc(sourceHandlers.List),
		SourceCreate: http.HandlerFunc(sourceHandlers.Create),
		SourceGet:    http.HandlerFunc(sourceHandlers.Get),
		SourceUpdate: http.HandlerFunc(sourceHandlers.Update),
		SourceDelete: http.HandlerFunc(sourceHandlers.Delete),
		SourceSync:   http.HandlerFunc(sourceHandlers.Sync),
		SourceTest:   http.HandlerFunc(sourceHandlers.Test),
	}

	return &apiServer{
		Handler: api.New(deps),
		pool:    pool,
		limiter: limiter,
		usage:   usageCounter,
	}, nil
}

// controlPlaneCheck is a readiness probe that pings the control-plane pool
// (SPEC-10 §4). A dead control plane flips /readyz to 503.
func controlPlaneCheck(pool *pgxpool.Pool) obs.Check {
	return obs.Check{
		Name: "control_plane",
		Probe: func(r *http.Request) error {
			return pool.Ping(r.Context())
		},
	}
}

// Close releases the control-plane pool.
func (s *apiServer) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}
