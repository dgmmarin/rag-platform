// Package tenant is the sole entry point for tenant data access (SPEC-01,
// ADR-0003). With one database per tenant (C-1, ADR-0001), a request or job must
// target exactly the right database; this package makes that structural.
//
// The only type that can execute SQL against a tenant database is *DB, and the
// only way to obtain one is Resolver.Open. The tenant ID also travels in
// context.Context — but for observability only (logs, metrics, tracing). A data
// handle is never stored in context (ADR-0003): forgetting the tenant is a
// compile error, not a runtime leak.
package tenant

import (
	"context"

	"github.com/google/uuid"
)

// ID identifies a tenant. It mirrors the control-plane tenants.id UUID.
type ID uuid.UUID

// String renders the tenant ID as its canonical UUID text.
func (id ID) String() string { return uuid.UUID(id).String() }

// ctxKey is a private context key type so no other package can collide with or
// read our context value by key.
type ctxKey struct{}

// WithTenantID returns a context carrying the tenant identity for logging,
// metrics labels and tracing attributes (SPEC-01 §5). It never carries a data
// handle (ADR-0003).
func WithTenantID(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// TenantIDFromCtx returns the tenant ID placed by WithTenantID, if any. The name
// is fixed by SPEC-01 §1 (paired with WithTenantID); the spec is the source of
// truth over the revive stutter heuristic.
//
//nolint:revive // spec-mandated name (SPEC-01 §1)
func TenantIDFromCtx(ctx context.Context) (ID, bool) {
	id, ok := ctx.Value(ctxKey{}).(ID)
	return id, ok
}
