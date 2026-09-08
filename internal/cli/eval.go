package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/eval"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// EvalCmd groups the per-tenant evaluation-harness case commands (STORY-12.1,
// FR-ADM-04). Running the cases (recall@k, grounded rate, latency) is
// `ragctl eval run`, delivered by STORY-12.2 and not wired here.
//
// Every subcommand takes a tenant --slug: the eval cases are tenant content
// reached through the resolver (ADR-0003, C-3), so the slug selects exactly one
// tenant database and there is no tenant_id parameter on the data.
type EvalCmd struct {
	Add    EvalAddCmd    `cmd:"" help:"Add an eval case."`
	List   EvalListCmd   `cmd:"" help:"List a tenant's eval cases."`
	Edit   EvalEditCmd   `cmd:"" help:"Edit an existing eval case (only the flags you pass change)."`
	Rm     EvalRmCmd     `cmd:"" help:"Remove an eval case."`
	Import EvalImportCmd `cmd:"" help:"Import eval cases from a CSV file (see docs/adr/0069)."`
	Run    EvalRunCmd    `cmd:"" help:"Run all eval cases and report recall@k, grounded rate and latency (SPEC-06 §8)."`
}

// evalService builds the eval Service for a tenant slug: it opens the
// control-plane pool, loads the startup DEK (to decrypt the tenant DB password),
// builds the resolver — the ONLY source of a tenant.DB (ADR-0003) — and looks up
// the tenant id from its slug. The returned cleanup closes the pool; the caller
// defers it. It fails closed on a missing control-plane URL or unknown slug.
func evalService(ctx context.Context, g *Globals, slug string) (*eval.Service, tenant.ID, func(), error) {
	if strings.TrimSpace(slug) == "" {
		return nil, tenant.ID{}, nil, fmt.Errorf("eval: --slug is required")
	}
	if g.ControlPlaneURL == "" {
		return nil, tenant.ID{}, nil, fmt.Errorf("eval: no control-plane URL (set --control-plane-url or CONTROL_PLANE_URL)")
	}
	cipher, err := LoadStartupKeyring(ctx, g.Secrets)
	if err != nil {
		return nil, tenant.ID{}, nil, err
	}
	pool, err := pgxpool.New(ctx, g.ControlPlaneURL)
	if err != nil {
		return nil, tenant.ID{}, nil, fmt.Errorf("eval: open control-plane pool: %w", err)
	}
	cleanup := func() { pool.Close() }

	var idStr string
	err = pool.QueryRow(ctx, `select id::text from tenants where slug = $1`, slug).Scan(&idStr)
	if errors.Is(err, pgx.ErrNoRows) {
		cleanup()
		return nil, tenant.ID{}, nil, fmt.Errorf("eval: unknown tenant slug %q", slug)
	}
	if err != nil {
		cleanup()
		return nil, tenant.ID{}, nil, fmt.Errorf("eval: look up tenant %q: %w", slug, err)
	}
	u, err := uuid.Parse(idStr)
	if err != nil {
		cleanup()
		return nil, tenant.ID{}, nil, fmt.Errorf("eval: tenant %q has an invalid id: %w", slug, err)
	}

	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher})
	svc := eval.NewService(resolver, eval.NewTenantStore())
	return svc, tenant.ID(u), cleanup, nil
}

// EvalAddCmd adds one eval case.
type EvalAddCmd struct {
	Slug           string   `help:"Tenant slug." required:""`
	Question       string   `help:"The evaluation question." required:""`
	ExpectedAnswer string   `help:"Expected answer (optional)." name:"expected-answer"`
	DocID          []string `help:"Expected document UUID (repeatable)." name:"doc-id"`
	Tag            []string `help:"Tag (repeatable)." name:"tag"`
}

// Run creates the case and prints its new id.
func (c *EvalAddCmd) Run(k *kong.Context, g *Globals) error {
	ctx := context.Background()
	svc, tid, cleanup, err := evalService(ctx, g, c.Slug)
	if err != nil {
		return err
	}
	defer cleanup()

	in := eval.CaseInput{
		Question:       c.Question,
		ExpectedDocIDs: c.DocID,
		Tags:           c.Tag,
	}
	if c.ExpectedAnswer != "" {
		in.ExpectedAnswer = &c.ExpectedAnswer
	}
	got, err := svc.Create(ctx, tid, in)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(k.Stdout, "ragctl eval add: created case %s\n", got.ID)
	return werr
}

// EvalListCmd lists a tenant's eval cases.
type EvalListCmd struct {
	Slug  string `help:"Tenant slug." required:""`
	Limit int    `help:"Maximum cases to list." default:"100"`
}

// Run prints one line per case: id, tags, #expected docs, and the question.
func (c *EvalListCmd) Run(k *kong.Context, g *Globals) error {
	ctx := context.Background()
	svc, tid, cleanup, err := evalService(ctx, g, c.Slug)
	if err != nil {
		return err
	}
	defer cleanup()

	cases, err := svc.List(ctx, tid, c.Limit)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		_, werr := fmt.Fprintln(k.Stdout, "ragctl eval list: no cases")
		return werr
	}
	for _, cs := range cases {
		tags := "-"
		if len(cs.Tags) > 0 {
			tags = strings.Join(cs.Tags, ",")
		}
		if _, werr := fmt.Fprintf(k.Stdout, "%s  [%s]  docs=%d  %s\n",
			cs.ID, tags, len(cs.ExpectedDocIDs), cs.Question); werr != nil {
			return werr
		}
	}
	return nil
}

// EvalEditCmd edits an existing case. Only the flags supplied change; the rest of
// the case is preserved (a get-then-merge).
//
// ponytail: a supplied --doc-id/--tag REPLACES the whole list; there is no way to
// clear a list back to empty from the CLI. Upgrade path: add --clear-tags /
// --clear-doc-ids flags if that is ever needed.
type EvalEditCmd struct {
	Slug           string   `help:"Tenant slug." required:""`
	ID             string   `help:"Eval case id (UUID)." required:""`
	Question       *string  `help:"New question."`
	ExpectedAnswer *string  `help:"New expected answer (pass empty string to clear)." name:"expected-answer"`
	DocID          []string `help:"Replace expected document UUIDs (repeatable)." name:"doc-id"`
	Tag            []string `help:"Replace tags (repeatable)." name:"tag"`
}

// Run merges the supplied flags onto the existing case and stores it.
func (c *EvalEditCmd) Run(k *kong.Context, g *Globals) error {
	ctx := context.Background()
	svc, tid, cleanup, err := evalService(ctx, g, c.Slug)
	if err != nil {
		return err
	}
	defer cleanup()

	cur, err := svc.Get(ctx, tid, c.ID)
	if err != nil {
		return err
	}

	in := eval.CaseInput{
		Question:       cur.Question,
		ExpectedAnswer: cur.ExpectedAnswer,
		ExpectedDocIDs: cur.ExpectedDocIDs,
		Tags:           cur.Tags,
	}
	if c.Question != nil {
		in.Question = *c.Question
	}
	if c.ExpectedAnswer != nil {
		if *c.ExpectedAnswer == "" {
			in.ExpectedAnswer = nil
		} else {
			in.ExpectedAnswer = c.ExpectedAnswer
		}
	}
	if c.DocID != nil {
		in.ExpectedDocIDs = c.DocID
	}
	if c.Tag != nil {
		in.Tags = c.Tag
	}

	got, err := svc.Update(ctx, tid, c.ID, in)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(k.Stdout, "ragctl eval edit: updated case %s\n", got.ID)
	return werr
}

// EvalRmCmd removes a case.
type EvalRmCmd struct {
	Slug string `help:"Tenant slug." required:""`
	ID   string `help:"Eval case id (UUID)." required:""`
}

// Run deletes the case, reporting a clear error when it does not exist.
func (c *EvalRmCmd) Run(k *kong.Context, g *Globals) error {
	ctx := context.Background()
	svc, tid, cleanup, err := evalService(ctx, g, c.Slug)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := svc.Delete(ctx, tid, c.ID); err != nil {
		return err
	}
	_, werr := fmt.Fprintf(k.Stdout, "ragctl eval rm: deleted case %s\n", c.ID)
	return werr
}

// EvalImportCmd imports eval cases from a CSV file (ADR-0069). The file is
// validated in full before anything is written; the import is atomic.
type EvalImportCmd struct {
	Slug string `help:"Tenant slug." required:""`
	File string `help:"CSV file to import (see docs/adr/0069 for the format)." required:"" type:"existingfile"`
}

// Run parses the CSV (the trust boundary), applies it in one transaction, and
// prints the created/updated counts.
func (c *EvalImportCmd) Run(k *kong.Context, g *Globals) error {
	ctx := context.Background()
	svc, tid, cleanup, err := evalService(ctx, g, c.Slug)
	if err != nil {
		return err
	}
	defer cleanup()

	f, err := os.Open(c.File)
	if err != nil {
		return fmt.Errorf("eval import: open %s: %w", c.File, err)
	}
	defer func() { _ = f.Close() }()

	res, err := svc.Import(ctx, tid, f)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(k.Stdout, "ragctl eval import: %d created, %d updated\n", res.Created, res.Updated)
	return werr
}
