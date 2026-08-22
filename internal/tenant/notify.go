package tenant

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantChangedChannel is the Postgres LISTEN/NOTIFY channel on which the control
// plane publishes a tenant's ID whenever its row changes (SPEC-01 §3). The
// resolver listens and invalidates that tenant's cached registry entry so a
// suspension or move takes effect within ~1s rather than on the 30s TTL.
const TenantChangedChannel = "tenant_changed"

// handleNotification parses a tenant_changed payload (a tenant UUID string) and
// invalidates that tenant in the registry. It is the pure core of the LISTEN
// loop, unit-tested without a database.
func handleNotification(reg Registry, payload string) error {
	u, err := uuid.Parse(payload)
	if err != nil {
		return fmt.Errorf("tenant: bad %s payload %q: %w", TenantChangedChannel, payload, err)
	}
	reg.Invalidate(ID(u))
	return nil
}

// listenTenantChanged runs the LISTEN loop until ctx is cancelled. It acquires a
// dedicated connection from the control pool, LISTENs on TenantChangedChannel,
// and invalidates the registry for each notification. Transient errors are
// returned so the caller can decide whether to restart the loop.
func listenTenantChanged(ctx context.Context, control *pgxpool.Pool, reg Registry) error {
	conn, err := control.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("tenant: acquire listen conn: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "listen "+TenantChangedChannel); err != nil {
		return fmt.Errorf("tenant: LISTEN %s: %w", TenantChangedChannel, err)
	}

	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("tenant: wait for notification: %w", err)
		}
		// A malformed payload must not kill the loop; skip it and keep listening.
		_ = handleNotification(reg, n.Payload)
	}
}
