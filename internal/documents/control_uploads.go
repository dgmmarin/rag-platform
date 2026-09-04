package documents

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ControlUploadSource resolves the tenant's implicit "upload" source over the
// control-plane pool (sources are control-plane registry data, C-3 — never a
// tenant database). The source is an ordinary sources row of kind 'upload';
// (tenant_id, name) is unique, so the upsert is idempotent and safe under
// concurrent uploads (SPEC-04 §5, STORY-06.3).
type ControlUploadSource struct{ pool *pgxpool.Pool }

// UploadSourceFromPool wraps a control-plane pool as an UploadSource.
func UploadSourceFromPool(pool *pgxpool.Pool) ControlUploadSource {
	return ControlUploadSource{pool: pool}
}

// uploadSourceName is the well-known name of a tenant's implicit upload source.
const uploadSourceName = "Uploaded documents"

// Resolve returns the tenant's upload source id, creating it on first use.
func (u ControlUploadSource) Resolve(ctx context.Context, tenantID string) (string, error) {
	var id string
	err := u.pool.QueryRow(ctx, `
		insert into sources (tenant_id, kind, name, status)
		values ($1, 'upload', $2, 'active')
		on conflict (tenant_id, name) do update set updated_at = now()
		returning id::text`,
		tenantID, uploadSourceName).Scan(&id)
	return id, err
}

// SettingsReader returns a tenant's resolved settings document (SPEC-02 §5).
// *tenants.SettingsService satisfies it structurally.
type SettingsReader interface {
	Get(ctx context.Context, tenantID string) (map[string]any, error)
}

// SettingsUploadLimits reads the per-tenant upload ceiling from settings
// (settings.limits.max_upload_mb, SPEC-02 §5), realising the UploadLimits port.
type SettingsUploadLimits struct{ Settings SettingsReader }

// MaxUploadBytes returns settings.limits.max_upload_mb in bytes, or 0 when it is
// absent/mis-typed so the service falls back to the configured global ceiling.
func (l SettingsUploadLimits) MaxUploadBytes(ctx context.Context, tenantID string) (int64, error) {
	doc, err := l.Settings.Get(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	mb, ok := maxUploadMB(doc)
	if !ok {
		return 0, nil
	}
	return int64(mb) << 20, nil
}

// maxUploadMB extracts settings.limits.max_upload_mb as an int (JSON numbers
// decode as float64).
func maxUploadMB(doc map[string]any) (int, bool) {
	limits, ok := doc["limits"].(map[string]any)
	if !ok {
		return 0, false
	}
	switch v := limits["max_upload_mb"].(type) {
	case float64:
		if v <= 0 {
			return 0, false
		}
		return int(v), true
	case int:
		if v <= 0 {
			return 0, false
		}
		return v, true
	default:
		return 0, false
	}
}
