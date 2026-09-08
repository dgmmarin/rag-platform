package eval

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// ErrTenantUnavailable wraps every resolver outcome meaning "the tenant is not
// ready" (provisioning/deleting/unknown/schema-behind, and a suspended tenant on
// a write).
var ErrTenantUnavailable = errors.New("eval: tenant unavailable")

// Service is the eval-harness domain logic. It owns the resolver (the ONLY source
// of a tenant.DB, ADR-0003) and the tenant-content Store. It is stateless and
// safe for concurrent use.
type Service struct {
	Resolver tenant.Resolver
	Store    Store
	// Runs persists eval_runs/eval_results (STORY-12.2). Optional: Run defaults it
	// to NewRunStore() when nil, so 12.1 callers are unaffected.
	Runs RunWriter
}

// NewService builds an eval service.
func NewService(resolver tenant.Resolver, store Store) *Service {
	return &Service{Resolver: resolver, Store: store}
}

// open resolves the tenant to its *tenant.DB, mapping resolver lifecycle outcomes
// to ErrTenantUnavailable (ADR-0003 — the only place a handle is obtained).
func (s *Service) open(ctx context.Context, tid tenant.ID) (*tenant.DB, error) {
	db, err := s.Resolver.Open(ctx, tid)
	if err != nil {
		switch {
		case errors.Is(err, tenant.ErrTenantUnavailable),
			errors.Is(err, tenant.ErrTenantNotFound),
			errors.Is(err, tenant.ErrSchemaOutdated):
			return nil, ErrTenantUnavailable
		default:
			return nil, fmt.Errorf("eval: open tenant: %w", err)
		}
	}
	return db, nil
}

// Create validates and stores a new case.
func (s *Service) Create(ctx context.Context, tid tenant.ID, in CaseInput) (Case, error) {
	if err := in.validate(); err != nil {
		return Case{}, err
	}
	db, err := s.open(ctx, tid)
	if err != nil {
		return Case{}, err
	}
	c, err := s.Store.Create(ctx, db, in)
	if err != nil {
		return Case{}, mapWrite(err)
	}
	return c, nil
}

// Get returns one case or ErrNotFound.
func (s *Service) Get(ctx context.Context, tid tenant.ID, id string) (Case, error) {
	db, err := s.open(ctx, tid)
	if err != nil {
		return Case{}, err
	}
	return s.Store.Get(ctx, db, id)
}

// List returns a page of cases (newest-first, capped).
func (s *Service) List(ctx context.Context, tid tenant.ID, limit int) ([]Case, error) {
	db, err := s.open(ctx, tid)
	if err != nil {
		return nil, err
	}
	return s.Store.List(ctx, db, limit)
}

// Update validates and applies changes to an existing case.
func (s *Service) Update(ctx context.Context, tid tenant.ID, id string, in CaseInput) (Case, error) {
	in.ID = "" // the path id is authoritative; validate the rest
	if err := in.validate(); err != nil {
		return Case{}, err
	}
	db, err := s.open(ctx, tid)
	if err != nil {
		return Case{}, err
	}
	c, err := s.Store.Update(ctx, db, id, in)
	if err != nil {
		return Case{}, mapWrite(err)
	}
	return c, nil
}

// Delete removes a case, returning ErrNotFound when it does not exist.
func (s *Service) Delete(ctx context.Context, tid tenant.ID, id string) error {
	db, err := s.open(ctx, tid)
	if err != nil {
		return err
	}
	existed, err := s.Store.Delete(ctx, db, id)
	if err != nil {
		return mapWrite(err)
	}
	if !existed {
		return ErrNotFound
	}
	return nil
}

// Import parses and validates a CSV stream (the trust boundary) and applies it in
// one transaction. Nothing is written if parsing/validation fails.
func (s *Service) Import(ctx context.Context, tid tenant.ID, r io.Reader) (ImportResult, error) {
	inputs, err := ParseCSV(r)
	if err != nil {
		return ImportResult{}, err
	}
	db, err := s.open(ctx, tid)
	if err != nil {
		return ImportResult{}, err
	}
	res, err := s.Store.Import(ctx, db, inputs)
	if err != nil {
		return ImportResult{}, mapWrite(err)
	}
	return res, nil
}

// Run executes the tenant's eval cases through the pipeline, recording an
// eval_run plus one eval_result per case, and returns the summary (recall@k,
// grounded rate, mean latency). It opens the tenant DB once (for the eval tables)
// via the resolver (ADR-0003); the pipeline resolves the tenant independently for
// retrieval/answering. A per-case pipeline error is fail-soft (SPEC-06 §8).
func (s *Service) Run(ctx context.Context, tid tenant.ID, opts RunOptions) (Summary, error) {
	if opts.Pipeline == nil {
		return Summary{}, fmt.Errorf("eval: run requires a pipeline")
	}
	db, err := s.open(ctx, tid)
	if err != nil {
		return Summary{}, err
	}
	runs := s.Runs
	if runs == nil {
		runs = NewRunStore()
	}
	runner := &Runner{
		Cases:    dbCaseSource{store: s.Store, db: db, limit: opts.Limit},
		Sink:     dbRunSink{runs: runs, db: db},
		Pipeline: opts.Pipeline,
		Judge:    opts.Judge,
		K:        opts.K,
	}
	summary, err := runner.Run(ctx, opts.Config)
	if err != nil {
		return Summary{}, mapWrite(err)
	}
	return summary, nil
}

// mapWrite turns a suspended-tenant write refusal into ErrTenantUnavailable and
// leaves domain sentinels (ErrNotFound) untouched.
func mapWrite(err error) error {
	if errors.Is(err, tenant.ErrReadOnly) {
		return ErrTenantUnavailable
	}
	if errors.Is(err, ErrNotFound) {
		return ErrNotFound
	}
	return fmt.Errorf("eval: %w", err)
}
