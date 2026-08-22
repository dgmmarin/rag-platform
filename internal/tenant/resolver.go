package tenant

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/migrate"
)

// expectedSchemaVersion is the tenant migration version the running binary
// requires. Open fails closed (ErrSchemaOutdated) against any tenant behind it
// (SPEC-01 §7). It is derived from the embedded tenant migrations (STORY-02.2)
// rather than hand-maintained, so it always tracks what `ragctl migrate tenants`
// applies and cannot silently drift; adding a migration bumps it automatically.
var expectedSchemaVersion = mustExpectedTenantVersion()

// mustExpectedTenantVersion reads the highest embedded tenant migration version.
// A build with no tenant migrations is a programming error, so it panics at init
// rather than defaulting to 0 (which would make Open reject every tenant).
func mustExpectedTenantVersion() int {
	v, err := migrate.ExpectedTenantVersion()
	if err != nil {
		panic(fmt.Sprintf("tenant: expected schema version: %v", err))
	}
	return int(v)
}

// Resolver maps a tenant ID to a *DB, applying the lifecycle status rules of
// SPEC-01 §3 and reusing per-tenant pools via a cache (SPEC-01 §4). It is the
// only source of a *DB (ADR-0003).
type Resolver interface {
	// Open resolves the tenant and returns a handle: read-write for active,
	// read-only for suspended, and an error for every other state.
	Open(ctx context.Context, id ID) (*DB, error)
	// Close evicts a tenant's cached pool (after delete or move; SPEC-01 §4).
	Close(id ID)
}

// resolver is the concrete Resolver: a registry (status + connection details)
// and a pool cache, plus the schema version it will serve.
type resolver struct {
	reg          Registry
	pools        *poolCache
	expectSchema int

	// control is the control-plane pool used to run the LISTEN loop for
	// tenant_changed. Nil in unit tests (they invalidate the registry directly).
	control *pgxpool.Pool
}

// newResolver wires a resolver from its collaborators. The build seam lets unit
// tests exercise resolution rules without dialing Postgres.
func newResolver(reg Registry, expectSchema int, build poolBuilder) *resolver {
	return &resolver{
		reg:          reg,
		pools:        newPoolCache(poolCacheOptions{build: build}),
		expectSchema: expectSchema,
	}
}

// Open implements Resolver.
func (r *resolver) Open(ctx context.Context, id ID) (*DB, error) {
	rec, err := r.reg.Lookup(ctx, id)
	if err != nil {
		return nil, err
	}

	readOnly := false
	switch rec.Status {
	case StatusActive:
		// read-write
	case StatusSuspended:
		readOnly = true
	case StatusProvisioning, StatusDeleting:
		return nil, ErrTenantUnavailable
	case StatusDeleted:
		return nil, ErrTenantNotFound
	default:
		return nil, ErrTenantNotFound
	}

	// Fail closed on schema drift before handing back a usable handle.
	if rec.SchemaVersion != r.expectSchema {
		return nil, ErrSchemaOutdated
	}

	pool, err := r.pools.get(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("tenant %s: open pool: %w", id, err)
	}
	return &DB{id: id, status: rec.Status, pool: pool, readOnly: readOnly}, nil
}

// Close implements Resolver: evict the tenant's pool and drop its cached record
// so the next Open reloads fresh connection details (tenant move, SPEC-01 §4).
func (r *resolver) Close(id ID) {
	r.pools.close(id)
	r.reg.Invalidate(id)
}

// closeAll releases all pools; used at shutdown.
func (r *resolver) closeAll() { r.pools.closeAll() }

// Decrypter opens an envelope-encrypted secret. *crypto.Cipher satisfies it; the
// tenant package depends on the behaviour, not the concrete type, so tests need
// no KMS setup.
type Decrypter interface {
	Decrypt(ciphertext []byte) ([]byte, error)
}

// Config wires a production Resolver.
type Config struct {
	// ControlPool is a pool to the control-plane database, used to load the
	// tenant registry (never for tenant data — C-3).
	ControlPool *pgxpool.Pool
	// Decrypter opens tenant_databases.password_enc at pool build time (SPEC-09).
	Decrypter Decrypter
	// CacheTTL is the registry cache lifetime (SPEC-01 §3: default 30s).
	CacheTTL time.Duration
}

// NewResolver builds a production Resolver backed by the control-plane database
// for the registry and a pool builder that decrypts each tenant's password and
// dials its dedicated database with the per-tenant role (SPEC-01 §4, SPEC-09).
func NewResolver(cfg Config) Resolver {
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	reg := newCachingRegistry(controlPlaneLoader(cfg.ControlPool), ttl, time.Now)
	build := passwordDecryptingBuilder(cfg.Decrypter)
	return &resolver{
		reg:          reg,
		pools:        newPoolCache(poolCacheOptions{build: build}),
		expectSchema: expectedSchemaVersion,
		control:      cfg.ControlPool,
	}
}

// Listen runs the tenant_changed LISTEN loop until ctx is cancelled, restarting
// after transient failures with a short backoff. Callers run it in a goroutine;
// it is how suspension/move take effect within ~1s instead of on the cache TTL
// (SPEC-01 §3). It is a no-op if no control pool was configured.
func (r *resolver) Listen(ctx context.Context) {
	if r.control == nil {
		return
	}
	for ctx.Err() == nil {
		if err := listenTenantChanged(ctx, r.control, r.reg); err != nil && ctx.Err() == nil {
			// Back off briefly before reconnecting so a flapping control plane
			// does not spin. The registry TTL still bounds staleness meanwhile.
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}
