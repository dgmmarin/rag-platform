package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/config"
	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/cp/audit"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/jobs"
	"github.com/rag-platform/ragctl/internal/cp/ratelimit"
	"github.com/rag-platform/ragctl/internal/cp/sources"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/cp/usage"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/documents"
	"github.com/rag-platform/ragctl/internal/llm"
	// Register the upload connector (kind "upload") into the default registry so
	// the sources API's config-validation / test-connection seams resolve it and
	// the "upload" kind is no longer an unregistered seam (SPEC-04 §5, STORY-06.3).
	_ "github.com/rag-platform/ragctl/internal/connector/upload"
	// Register the web_crawl AND sitemap connectors (SPEC-04 §2/§3, STORY-07.1/07.5):
	// both kinds live in and register from the webcrawl package (the sitemap connector
	// drives the same crawl core, ADR-0047), so this one blank import wires both — the
	// sources API resolves their JSON-Schema config validation and test-connection.
	_ "github.com/rag-platform/ragctl/internal/connector/webcrawl"
	// Register the HTTP API connector (kind "api", SPEC-04 §4, STORY-07.6): auth
	// (api_key_header/bearer/basic/oauth2_cc), pagination (none/page/offset/cursor/
	// link-header) and rate-limit/Retry-After handling. One blank import wires it —
	// no other change to the sources API or router (NFR-MNT-01).
	_ "github.com/rag-platform/ragctl/internal/connector/api"
	"github.com/rag-platform/ragctl/internal/objectstore"
	"github.com/rag-platform/ragctl/internal/obs"
	"github.com/rag-platform/ragctl/internal/provision"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
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

	pool, err := pgxpool.New(ctx, controlURL)
	if err != nil {
		return nil, fmt.Errorf("serve: open control-plane pool: %w", err)
	}

	// --- Tenant resolver: the ONLY source of a tenant.DB (ADR-0003). The documents
	// routes (STORY-04.4) read tenant content through it; the cipher decrypts each
	// tenant's stored DB password at pool-build time (SPEC-09 §2). ---
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher})

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
	// connector-framework Validator (EPIC-06 STORY-06.1) is now wired: create/update
	// run the kind-specific ValidateConfig for any registered connector, and /test
	// runs its "test connection". No connector is registered in v1 yet (upload is
	// STORY-06.3, web_crawl/api/sitemap are EPIC-07), so an unregistered kind defers
	// config validation (generic validation still applies) and /test reports the
	// not_found seam envelope (connector.ErrUnsupportedKind -> ErrConnectorUnavailable).
	// Adding a connector needs only its package + Register call — no change here
	// (NFR-MNT-01). Sources are control-plane registry data (C-3): control-plane pool,
	// never a tenant pool. ---
	sourcesSvc := sources.NewService(sources.FromPool(pool))
	sourcesSvc.Validator = connector.NewSourcesValidator(connector.DefaultRegistry(), sources.ErrConnectorUnavailable)
	// Credentials (FR-SRC-10, SPEC-04 §6): sealed on write and decrypted only for a
	// Test/Sync with the same platform Cipher the resolver/provisioner use (envelope
	// encryption, SPEC-09 §2, C-4). Never returned by any response, never logged.
	sourcesSvc.Encrypter = cipher
	sourcesSvc.Decrypter = cipher
	sourceHandlers := sources.NewHandlers(sourcesSvc)

	// Settings service (control-plane): used by the rate limiter, the admin-tenant
	// surface, and the per-tenant upload ceiling (STORY-06.3).
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))

	// --- Documents (tenant-content list/get/chunks/soft-delete + upload,
	// STORY-04.4/06.3). Reads reach the tenant database via the resolver
	// (ADR-0003, C-3); the ingest_document enqueue writes the control-plane jobs
	// table. STORY-06.3 wires the three upload seams: object storage for the raw
	// bytes (MinIO/S3), the implicit upload-source resolver, and the per-tenant
	// size ceiling from settings. Object storage is optional — an unset endpoint (or
	// an unreachable store at boot) leaves Storage nil so uploads report the
	// not_found seam while reads keep working. ---
	docSvc := documents.NewService(resolver, documents.NewTenantStore(), documents.JobsFromPool(pool))
	docSvc.MaxBytes = cfg.MaxUploadBytes
	docSvc.UploadSource = documents.UploadSourceFromPool(pool)
	docSvc.Limits = documents.SettingsUploadLimits{Settings: settingsSvc}
	if cfg.ObjectStoreEndpoint != "" {
		store, oerr := objectstore.New(ctx, objectstore.Config{
			Endpoint:  cfg.ObjectStoreEndpoint,
			AccessKey: cfg.ObjectStoreAccessKey,
			SecretKey: cfg.ObjectStoreSecretKey,
			Bucket:    cfg.ObjectStoreBucket,
			Region:    cfg.ObjectStoreRegion,
		})
		if oerr != nil {
			// Do not couple API liveness to object storage: log and leave uploads on
			// the seam. Reads (which never touch object storage) keep working.
			log.Warn("object storage unavailable; POST /v1/documents will report the not_found seam", "err", oerr)
		} else {
			docSvc.Storage = store
		}
	}
	docHandlers := documents.NewHandlers(docSvc)

	// --- Jobs (tenant-scoped list/get/cancel over the control-plane jobs table,
	// STORY-04.5). Jobs are the control-plane history/mirror view (C-3), so this
	// uses the control-plane pool — never a tenant pool. Cancelling a QUEUED job is
	// fully effective now; cancelling a RUNNING job needs the River worker
	// (EPIC-09), left as the nil Canceller seam (returns the not_found seam
	// envelope until wired). See ADR-0031. ---
	jobHandlers := jobs.NewHandlers(jobs.NewService(jobs.FromPool(pool)))

	// --- Retrieve (tenant-scoped hybrid retrieval, STORY-08.2, FR-RET-08). Reads
	// tenant content through the resolver (ADR-0003, C-3). The incoming query is
	// embedded with the tenant's configured provider (the same seam ingest uses —
	// query and corpus share an embedding space), authenticated with the platform
	// embedding key; embed.New fails closed on the tenant's providers_allowed
	// (SPEC-09 §2). `query` scope. Returns raw fused results — reranking/answering
	// layer on later (08.3/08.5). ---
	retrieveSvc := retrieve.NewService(resolver, settingsSvc,
		retrieve.KeyedEmbedderFactory{APIKey: cfg.EmbeddingAPIKey, BaseURL: cfg.EmbeddingBaseURL})
	// Reranking (STORY-08.3, SPEC-06 §3, FR-RET-03). Off by default per tenant
	// (settings.reranker.enabled); when on, the fused top_n are reranked (Cohere or
	// an LLM listwise call) and reordered by reranker score. The Cohere key is the
	// platform COHERE_API_KEY (fail-closed on providers_allowed inside rerank.New);
	// the LLM reranker reuses the tenant's llm.Provider built from the per-provider
	// LLM keys (allowlist enforced by llm.New). Any reranker failure falls back to
	// fused order — the query never fails on it (NFR-REL-04).
	retrieveSvc.Reranker = retrieve.KeyedRerankerFactory{
		CohereAPIKey:  cfg.CohereAPIKey,
		CohereBaseURL: cfg.CohereBaseURL,
		LLM: llm.Factory{Keys: llm.Keys{
			Anthropic:     cfg.AnthropicAPIKey,
			OpenAI:        cfg.OpenAIAPIKey,
			OpenAIBaseURL: cfg.OpenAIBaseURL,
		}},
	}
	retrieveHandlers := retrieve.NewHandlers(retrieveSvc)

	// --- Rate limiting (per key + per tenant, credential-keyed). ---
	limiter := ratelimit.New(nil)

	// --- Admin tenant lifecycle (platform scope, STORY-04.6, FR-TEN-01/05/07). It
	// orchestrates the existing provisioner (enrol) and lifecycle (suspend/resume/
	// move/schedule-delete) plus the settings service. Provisioning/lifecycle use a
	// privileged (superuser) connection — PROVISION_DB_URL, falling back to the
	// control-plane URL, exactly as `ragctl enroll`/`tenant` resolve it (STORY-02.3/
	// 02.4/02.5). The cipher seals the generated/rotated tenant DB password
	// (SPEC-09 §2, C-4). Registry reads use the control-plane pool (C-3). Async
	// River provision_tenant/delete_tenant execution is EPIC-09; provisioning runs
	// synchronously today (ADR-0016). ---
	privilegedURL := cfg.ProvisionURL
	if privilegedURL == "" {
		privilegedURL = controlURL
	}
	adminSvc := &tenants.AdminService{
		Store: tenants.AdminStoreFromPool(pool),
		Prov: &provision.Provisioner{
			PrivilegedURL: privilegedURL,
			Encrypter:     cipher,
			Decrypter:     cipher,
			TenantHost:    cfg.TenantDBHost,
			TenantPort:    cfg.TenantDBPort,
		},
		Life:     &provision.Lifecycle{PrivilegedURL: privilegedURL, Encrypter: cipher},
		Settings: settingsSvc,
		SSLMode:  cfg.TenantDBSSLMode,
	}
	tenantHandlers := tenants.NewAdminHandlers(adminSvc)
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

		TenantCreate: http.HandlerFunc(tenantHandlers.Create),
		TenantList:   http.HandlerFunc(tenantHandlers.List),
		TenantUpdate: http.HandlerFunc(tenantHandlers.Update),
		TenantDelete: http.HandlerFunc(tenantHandlers.Delete),

		SourceList:   http.HandlerFunc(sourceHandlers.List),
		SourceCreate: http.HandlerFunc(sourceHandlers.Create),
		SourceGet:    http.HandlerFunc(sourceHandlers.Get),
		SourceUpdate: http.HandlerFunc(sourceHandlers.Update),
		SourceDelete: http.HandlerFunc(sourceHandlers.Delete),
		SourceSync:   http.HandlerFunc(sourceHandlers.Sync),
		SourceTest:   http.HandlerFunc(sourceHandlers.Test),

		DocumentIngest: http.HandlerFunc(docHandlers.Ingest),
		DocumentList:   http.HandlerFunc(docHandlers.List),
		DocumentGet:    http.HandlerFunc(docHandlers.Get),
		DocumentDelete: http.HandlerFunc(docHandlers.Delete),
		DocumentChunks: http.HandlerFunc(docHandlers.Chunks),

		JobList:   http.HandlerFunc(jobHandlers.List),
		JobGet:    http.HandlerFunc(jobHandlers.Get),
		JobCancel: http.HandlerFunc(jobHandlers.Cancel),

		Retrieve: http.HandlerFunc(retrieveHandlers.Retrieve),
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
