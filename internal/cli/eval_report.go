package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/alecthomas/kong"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/eval"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// EvalReportCmd prints a stored eval run and its per-case results as JSON
// (STORY-12.4, FR-ADM-04): the machine-readable data contract the EPIC-11 admin UI
// will render. It reads eval_runs/eval_results (tenant content) through the
// resolver (ADR-0003, C-3). No admin UI is built here — this is the data layer
// underneath it.
type EvalReportCmd struct {
	Slug  string `arg:"" help:"Tenant slug."`
	RunID string `arg:"" help:"Eval run id (UUID)." name:"run-id"`
}

// Run resolves the tenant, reads the run + results, and writes the report JSON.
func (c *EvalReportCmd) Run(k *kong.Context, g *Globals) error {
	ctx := context.Background()

	if g.ControlPlaneURL == "" {
		return fmt.Errorf("eval report: no control-plane URL (set --control-plane-url or CONTROL_PLANE_URL)")
	}
	cipher, err := LoadStartupKeyring(ctx, g.Secrets)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, g.ControlPlaneURL)
	if err != nil {
		return fmt.Errorf("eval report: open control-plane pool: %w", err)
	}
	defer pool.Close()

	tid, err := tenantIDForSlug(ctx, pool, c.Slug)
	if err != nil {
		return err
	}

	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher})
	svc := eval.NewService(resolver, eval.NewTenantStore())
	svc.Runs = eval.NewRunStore()

	report, err := svc.Report(ctx, tid, c.RunID)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintln(k.Stdout, string(body))
	return werr
}
