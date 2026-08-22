package tenant

import "errors"

// Resolution errors returned by Resolver.Open, mapping tenant lifecycle status
// to a caller-visible outcome (SPEC-01 §3). Callers match with errors.Is and
// translate to the appropriate API status (e.g. tenant_unavailable, 404).
var (
	// ErrReadOnly is returned by DB.Exec/DB.Begin when the tenant is suspended
	// (SPEC-01 §3): reads are allowed, writes are refused.
	ErrReadOnly = errors.New("tenant: database is read-only (tenant suspended)")

	// ErrTenantUnavailable indicates the tenant exists but is not ready to serve
	// (provisioning or deleting).
	ErrTenantUnavailable = errors.New("tenant: unavailable")

	// ErrTenantNotFound indicates the tenant is unknown or has been deleted.
	ErrTenantNotFound = errors.New("tenant: not found")

	// ErrSchemaOutdated indicates the tenant database is behind the expected
	// migration version; Open fails closed rather than serving a stale schema
	// (SPEC-01 §7).
	ErrSchemaOutdated = errors.New("tenant: schema version outdated")
)
