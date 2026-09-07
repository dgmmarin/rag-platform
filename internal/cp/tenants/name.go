package tenants

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// NameService resolves a tenant's display name (tenants.name) for the answering
// stage's refusal message (SPEC-06 §4, "…in <tenant name>'s content."). The name is
// control-plane registry data, not tenant content (C-3), so this is a plain
// control-plane read — it never opens a tenant database. It reuses the SettingsDB
// seam (QueryRow), so a pgx pool adapter (SettingsFromPool) satisfies it and unit
// tests supply a fake.
type NameService struct {
	DB SettingsDB
}

// NewNameService builds a NameService over the given DB.
func NewNameService(db SettingsDB) *NameService { return &NameService{DB: db} }

// Name returns the tenant's display name, or ErrTenantNotFound when no row matches.
func (s *NameService) Name(ctx context.Context, tenantID string) (string, error) {
	var name string
	err := s.DB.QueryRow(ctx, `select name from tenants where id = $1`, tenantID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTenantNotFound
	}
	if err != nil {
		return "", err
	}
	return name, nil
}
