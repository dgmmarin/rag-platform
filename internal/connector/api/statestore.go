package api

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// tenantStateStore is the durable connector.StateStore backing for one source: a
// generic per-source key/value store in the tenant `connector_state` table, reached
// ONLY through a resolved *tenant.DB (ADR-0003, C-3). The tenant DB never holds a
// tenant_id column (C-1); source_id is an informational copy of a control-plane id
// (SPEC-03 §2 invariant 4), so there is no cross-database FK.
//
// The API connector uses it for the incremental cursor and the last-full-sync
// marker (SPEC-04 §4). It is generic (not API-specific): any future connector that
// needs simple per-source scratch can reuse the table and this store. The worker
// (EPIC-09) constructs one per run and sets it as SyncRun.State; unit tests use an
// in-memory StateStore instead (see incremental_test.go), so this type is exercised
// by the DB-backed e2e (test/e2e/api_e2e_test.go).
type tenantStateStore struct {
	db       *tenant.DB
	sourceID uuid.UUID
}

// NewTenantStateStore builds the connector_state-backed StateStore for one source.
func NewTenantStateStore(db *tenant.DB, sourceID uuid.UUID) connector.StateStore {
	return &tenantStateStore{db: db, sourceID: sourceID}
}

func (s *tenantStateStore) Get(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(ctx,
		`select value from connector_state where source_id = $1 and key = $2`,
		s.sourceID, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (s *tenantStateStore) Set(ctx context.Context, key, value string) error {
	_, err := s.db.Exec(ctx, `
		insert into connector_state (source_id, key, value, updated_at)
		values ($1, $2, $3, now())
		on conflict (source_id, key) do update set
		    value      = excluded.value,
		    updated_at = now()`,
		s.sourceID, key, value)
	return err
}
