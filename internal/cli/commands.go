package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/alecthomas/kong"

	"github.com/rag-platform/ragctl/internal/migrate"
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

// MigrateTenantsCmd applies tenant migrations (STORY-02.2).
type MigrateTenantsCmd struct {
	Parallel int    `help:"Number of tenants to migrate in parallel." default:"4"`
	Tenant   string `help:"Restrict to a single tenant slug."`
}

// Run is a STORY-01.1 stub; STORY-02.2 wires the per-tenant runner.
func (c *MigrateTenantsCmd) Run(k *kong.Context) error {
	return stub(k.Stdout, "ragctl migrate tenants: tenant migrations")
}

// EnrollCmd enrols a new tenant (SPEC-02 §7:
// `ragctl enroll --slug acme --name "Acme Inc" --region eu-central --db-host pg-1`).
type EnrollCmd struct {
	Slug   string `help:"Tenant slug." required:""`
	Name   string `help:"Tenant display name." required:""`
	Region string `help:"Deployment region."`
	DBHost string `help:"Database host for the tenant." name:"db-host"`
}

// Run is a STORY-01.1 stub; STORY-02.3 enqueues provision_tenant.
func (c *EnrollCmd) Run(k *kong.Context) error {
	return stub(k.Stdout, "ragctl enroll: tenant %q (%s)", c.Slug, c.Name)
}
