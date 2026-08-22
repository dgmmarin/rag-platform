package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/rag-platform/ragctl/internal/migrate"
	"github.com/rag-platform/ragctl/internal/provision"
)

// stub writes a recognisable "not implemented" line for a wired-but-unfinished
// command and returns ErrNotImplemented. A write failure is surfaced so callers
// do not mistake a broken stdout for a clean run.
func stub(w io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(w, format+" (not implemented)\n", args...); err != nil {
		return err
	}
	return ErrNotImplemented
}

// ServeCmd starts the HTTP API (SPEC-02 §7: `ragctl serve --addr :8080`).
type ServeCmd struct {
	Addr string `help:"Listen address for the HTTP API." env:"RAGCTL_ADDR" default:":8080"`
}

// Run loads the data-encryption key at startup and fails closed if it is
// missing or cannot be unwrapped (STORY-01.4, SPEC-09 §2), then starts the
// scaffolding HTTP server: /healthz, /readyz and /metrics behind the obs
// middleware, with the OTel tracer configured by env (STORY-01.6, SPEC-10). It
// blocks until SIGINT/SIGTERM triggers a graceful shutdown. STORY-04.1 replaces
// the minimal server with the full router/middleware chain.
//
// Structured logs go to stderr so they never intermix with a command's stdout
// output. The DEK the cipher holds will encrypt/decrypt tenant secrets the
// server later handles.
func (c *ServeCmd) Run(k *kong.Context, g *Globals) error {
	if _, err := LoadStartupCipher(context.Background(), g.Secrets); err != nil {
		return err
	}
	return runServer(context.Background(), c.Addr, g.Obs, k.Stderr)
}

// WorkCmd starts the job worker (SPEC-02 §7:
// `ragctl work --queues ingest,maintenance,platform`).
type WorkCmd struct {
	Queues []string `help:"Queues to consume." env:"RAGCTL_QUEUES" default:"ingest,maintenance,platform"`
}

// Run is a STORY-01.1 stub; STORY-09.1 replaces it with the River worker.
func (c *WorkCmd) Run(k *kong.Context) error {
	return stub(k.Stdout, "ragctl work: worker on queues %v", c.Queues)
}

// MigrateCmd groups migration subcommands (SPEC-02 §7).
type MigrateCmd struct {
	Control MigrateControlCmd `cmd:"" help:"Apply control-plane migrations."`
	Tenants MigrateTenantsCmd `cmd:"" help:"Apply tenant migrations to all tenants."`
}

// MigrateControlCmd applies control-plane migrations (STORY-01.5).
type MigrateControlCmd struct{}

// Run applies the embedded goose control-plane migrations against the resolved
// control-plane URL (STORY-01.5, SPEC-02 §7). It is idempotent.
func (c *MigrateControlCmd) Run(k *kong.Context, g *Globals) error {
	if g.ControlPlaneURL == "" {
		return fmt.Errorf("migrate control: no control-plane URL (set --control-plane-url or CONTROL_PLANE_URL)")
	}
	if err := migrate.Control(context.Background(), g.ControlPlaneURL); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(k.Stdout, "ragctl migrate control: applied control-plane migrations"); err != nil {
		return err
	}
	return nil
}

// MigrateTenantsCmd applies tenant migrations (STORY-02.2, SPEC-01 §7).
type MigrateTenantsCmd struct {
	Parallel int    `help:"Number of tenants to migrate in parallel." default:"4"`
	Tenant   string `help:"Restrict to a single tenant slug."`
}

// Run applies pending tenant migrations to every eligible tenant in parallel,
// recording each applied schema_version, continuing past per-tenant failures,
// and exiting non-zero with the list of failed tenants so a rerun resumes only
// those (STORY-02.2, FR-TEN-09, SPEC-01 §7). It loads the startup DEK so it can
// decrypt each tenant's stored database password (SPEC-09 §2).
func (c *MigrateTenantsCmd) Run(k *kong.Context, g *Globals) error {
	if g.ControlPlaneURL == "" {
		return fmt.Errorf("migrate tenants: no control-plane URL (set --control-plane-url or CONTROL_PLANE_URL)")
	}
	cipher, err := LoadStartupCipher(context.Background(), g.Secrets)
	if err != nil {
		return err
	}

	res, err := migrate.Tenants(context.Background(), g.ControlPlaneURL, migrate.TenantOptions{
		Parallel:   c.Parallel,
		Decrypter:  cipher,
		OnlyTenant: c.Tenant,
	})
	if err != nil {
		return err
	}

	for _, o := range res.Migrated {
		if _, werr := fmt.Fprintf(k.Stdout, "ragctl migrate tenants: %s ok (version %d)\n", o.Slug, o.Version); werr != nil {
			return werr
		}
	}
	for _, o := range res.Failed {
		if _, werr := fmt.Fprintf(k.Stderr, "ragctl migrate tenants: %s FAILED: %v\n", o.Slug, o.Err); werr != nil {
			return werr
		}
	}
	if res.HasFailures() {
		slugs := make([]string, 0, len(res.Failed))
		for _, o := range res.Failed {
			slugs = append(slugs, o.Slug)
		}
		return fmt.Errorf("migrate tenants: %d tenant(s) failed: %s", len(res.Failed), strings.Join(slugs, ", "))
	}
	if _, werr := fmt.Fprintf(k.Stdout, "ragctl migrate tenants: applied to %d tenant(s)\n", len(res.Migrated)); werr != nil {
		return werr
	}
	return nil
}

// EnrollCmd enrols a new tenant (SPEC-02 §7:
// `ragctl enroll --slug acme --name "Acme Inc" --region eu-central --db-host pg-1`).
type EnrollCmd struct {
	Slug      string `help:"Tenant slug." required:""`
	Name      string `help:"Tenant display name." required:""`
	Region    string `help:"Deployment region."`
	DBHost    string `help:"Database host to record for the tenant (overrides TENANT_DB_HOST)." name:"db-host"`
	DBPort    int    `help:"Database port to record for the tenant (overrides TENANT_DB_PORT)." name:"db-port"`
	DBSSLMode string `help:"sslmode recorded for the tenant DB (default require; use disable locally)." name:"db-ssl-mode"`
	Dimension int    `help:"Embedding vector dimension for the tenant (default 1536)." name:"embedding-dim"`
}

// Run provisions the tenant synchronously (STORY-02.3, FR-TEN-01/02, SPEC-01 §6):
// it creates the least-privilege role + database, installs the required
// extensions on the privileged connection, encrypts and records the connection
// details, applies the tenant migrations, and sets the tenant active. The
// asynchronous `provision_tenant` job enqueue is deferred to EPIC-09 (River,
// ADR-0005); until then enroll runs the same idempotent handler directly so a
// retry is safe.
//
// The privileged connection comes from PROVISION_DB_URL, falling back to the
// control-plane URL. The DEK is loaded at startup to encrypt the generated
// password with the same Cipher the resolver decrypts with (SPEC-09 §2).
func (c *EnrollCmd) Run(k *kong.Context, g *Globals) error {
	privileged := g.ProvisionURL
	if privileged == "" {
		privileged = g.ControlPlaneURL
	}
	if privileged == "" {
		return fmt.Errorf("enroll: no provisioning URL (set --control-plane-url/CONTROL_PLANE_URL or PROVISION_DB_URL)")
	}

	cipher, err := LoadStartupCipher(context.Background(), g.Secrets)
	if err != nil {
		return err
	}

	host := c.DBHost
	if host == "" {
		host = g.TenantDBHost
	}
	port := c.DBPort
	if port == 0 {
		port = g.TenantDBPort
	}
	sslMode := c.DBSSLMode
	if sslMode == "" {
		sslMode = g.TenantDBSSLMode
	}

	p := &provision.Provisioner{
		PrivilegedURL: privileged,
		Encrypter:     cipher,
		Decrypter:     cipher,
		TenantHost:    host,
		TenantPort:    port,
	}
	res, err := p.Provision(context.Background(), provision.Params{
		Slug:         c.Slug,
		Name:         c.Name,
		Region:       c.Region,
		Host:         host,
		Port:         port,
		SSLMode:      sslMode,
		EmbeddingDim: c.Dimension,
	})
	if err != nil {
		return err
	}
	if _, werr := fmt.Fprintf(k.Stdout,
		"ragctl enroll: tenant %q provisioned (id %s, database %s, schema version %d)\n",
		res.Slug, res.TenantID, res.DatabaseName, res.SchemaVersion); werr != nil {
		return werr
	}
	return nil
}

// TenantCmd groups tenant lifecycle subcommands (STORY-02.4, SPEC-01 §8).
type TenantCmd struct {
	Suspend TenantSuspendCmd `cmd:"" help:"Suspend a tenant (reads only; end-user query/ingest refused)."`
	Resume  TenantResumeCmd  `cmd:"" help:"Resume a suspended tenant."`
	Delete  TenantDeleteCmd  `cmd:"" help:"Schedule, cancel, or run a tenant deletion."`
	Move    TenantMoveCmd    `cmd:"" help:"Update a tenant's database connection (tenant move)."`
}

// lifecycleForGlobals builds the Lifecycle service from resolved globals, applying
// the same privileged-connection resolution as enroll (PROVISION_DB_URL, falling
// back to the control-plane URL). It fails closed with an actionable error when
// neither is set.
func lifecycleForGlobals(g *Globals) (*provision.Lifecycle, error) {
	privileged := g.ProvisionURL
	if privileged == "" {
		privileged = g.ControlPlaneURL
	}
	if privileged == "" {
		return nil, fmt.Errorf("tenant: no provisioning URL (set --control-plane-url/CONTROL_PLANE_URL or PROVISION_DB_URL)")
	}
	return &provision.Lifecycle{PrivilegedURL: privileged}, nil
}

// TenantSuspendCmd suspends a tenant (FR-TEN-04).
type TenantSuspendCmd struct {
	Slug string `help:"Tenant slug." required:""`
}

// Run moves the tenant active -> suspended so viewers/API keys are refused while
// admins can still read (FR-TEN-04, SPEC-01 §3/§8). The control-plane status
// write fires tenant_changed so the resolver invalidates its cache within ~1s.
func (c *TenantSuspendCmd) Run(k *kong.Context, g *Globals) error {
	l, err := lifecycleForGlobals(g)
	if err != nil {
		return err
	}
	res, err := l.Suspend(context.Background(), c.Slug)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(k.Stdout, "ragctl tenant suspend: %q %s -> %s\n", res.Slug, res.FromStatus, res.ToStatus)
	return werr
}

// TenantResumeCmd resumes a suspended tenant.
type TenantResumeCmd struct {
	Slug string `help:"Tenant slug." required:""`
}

// Run moves the tenant suspended -> active (SPEC-01 §8).
func (c *TenantResumeCmd) Run(k *kong.Context, g *Globals) error {
	l, err := lifecycleForGlobals(g)
	if err != nil {
		return err
	}
	res, err := l.Resume(context.Background(), c.Slug)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(k.Stdout, "ragctl tenant resume: %q %s -> %s\n", res.Slug, res.FromStatus, res.ToStatus)
	return werr
}

// TenantMoveCmd updates a tenant's database connection record (FR-TEN-07,
// SPEC-01 §4). Only the flags supplied change; a new password is re-encrypted
// with the platform DEK so it round-trips through the resolver's decrypt
// (SPEC-09 §2). The control-plane write fires tenant_changed, so the resolver
// evicts the pool and rebuilds against the new connection on the next Open —
// this is the eviction the AC calls for (STORY-02.5).
//
// The HTTP `PATCH /admin/tenants/{id}` route in the AC is deferred to EPIC-04
// (STORY-04.6): the public router does not exist yet (STORY-04.1). Until then
// this command is the sole entry point, exactly as enroll/suspend/delete are for
// their operations (ADR-0016/0017). A move only repoints the registry; the
// operator copies the database out of band first (docs/runbooks/move-tenant.md).
type TenantMoveCmd struct {
	Slug       string `help:"Tenant slug." required:""`
	DBHost     string `help:"New database host." name:"db-host"`
	DBPort     int    `help:"New database port." name:"db-port"`
	DBName     string `help:"New database name." name:"db-name"`
	DBUser     string `help:"New per-tenant role/username." name:"db-user"`
	DBSSLMode  string `help:"New sslmode (require, verify-full, disable)." name:"db-ssl-mode"`
	DBPassword string `help:"New database password (re-encrypted before storage; prefer DB_PASSWORD env)." name:"db-password" env:"DB_PASSWORD"`
}

// Run loads the DEK (needed to seal a rotated password), builds the Lifecycle
// with an Encrypter, and applies the connection update. It prints only non-secret
// metadata; the password is never echoed.
func (c *TenantMoveCmd) Run(k *kong.Context, g *Globals) error {
	l, err := lifecycleForGlobals(g)
	if err != nil {
		return err
	}
	cipher, err := LoadStartupCipher(context.Background(), g.Secrets)
	if err != nil {
		return err
	}
	l.Encrypter = cipher

	res, err := l.Move(context.Background(), c.Slug, provision.MoveParams{
		Host:     c.DBHost,
		Port:     c.DBPort,
		Database: c.DBName,
		Username: c.DBUser,
		SSLMode:  c.DBSSLMode,
		Password: c.DBPassword,
	})
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(k.Stdout,
		"ragctl tenant move: %q connection updated (pool will rebuild on next request)\n", res.Slug)
	return werr
}

// TenantDeleteCmd schedules a deletion (default), cancels a pending one
// (--cancel), or performs the irreversible teardown once the grace period has
// elapsed (--run) (FR-TEN-05, SPEC-01 §8).
//
// Async scheduling via a River `delete_tenant` job (ADR-0005) is deferred to
// EPIC-09; until then --run invokes the same idempotent handler directly, exactly
// as enroll does for provisioning (ADR-0016). The grace deadline is honoured
// in-handler: --run refuses to drop before delete_after.
type TenantDeleteCmd struct {
	Slug    string        `help:"Tenant slug." required:""`
	Grace   time.Duration `help:"Grace period before the deletion is irreversible (default 168h = 7 days)." default:"168h"`
	Cancel  bool          `help:"Cancel a pending deletion during its grace period."`
	Execute bool          `name:"run" help:"Run the irreversible teardown now (only after the grace period)."`
}

// Run dispatches to schedule / cancel / run based on the flags. --cancel and
// --run are mutually exclusive.
func (c *TenantDeleteCmd) Run(k *kong.Context, g *Globals) error {
	if c.Cancel && c.Execute {
		return fmt.Errorf("tenant delete: --cancel and --run are mutually exclusive")
	}
	l, err := lifecycleForGlobals(g)
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch {
	case c.Cancel:
		res, err := l.CancelDelete(ctx, c.Slug)
		if err != nil {
			return err
		}
		_, werr := fmt.Fprintf(k.Stdout, "ragctl tenant delete: cancelled %q (restored to %s)\n", res.Slug, res.ToStatus)
		return werr
	case c.Execute:
		res, err := l.RunDelete(ctx, c.Slug)
		if err != nil {
			return err
		}
		if res.ObjectStoreDeferred {
			if _, werr := fmt.Fprintf(k.Stderr,
				"ragctl tenant delete: object-storage prefix removal deferred (no client configured; EPIC-06)\n"); werr != nil {
				return werr
			}
		}
		_, werr := fmt.Fprintf(k.Stdout, "ragctl tenant delete: teardown complete for %q (database and role dropped)\n", res.Slug)
		return werr
	default:
		res, err := l.ScheduleDelete(ctx, c.Slug, c.Grace)
		if err != nil {
			return err
		}
		_, werr := fmt.Fprintf(k.Stdout,
			"ragctl tenant delete: scheduled %q for deletion after %s (cancel with --cancel until then)\n",
			res.Slug, res.DeleteAfter.UTC().Format(time.RFC3339))
		return werr
	}
}
