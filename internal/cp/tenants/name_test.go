package tenants

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// nameFakeDB is a minimal SettingsDB whose QueryRow scans a canned name (or a
// canned error) so the NameService branch logic is unit-tested without Postgres.
type nameFakeDB struct {
	name string
	err  error
}

func (f nameFakeDB) QueryRow(_ context.Context, _ string, _ ...any) rowScanner {
	return nameFakeRow{f: f}
}
func (f nameFakeDB) Exec(context.Context, string, ...any) (commandTag, error) {
	return fakeTag{}, nil
}

type nameFakeRow struct{ f nameFakeDB }

func (r nameFakeRow) Scan(dest ...any) error {
	if r.f.err != nil {
		return r.f.err
	}
	*(dest[0].(*string)) = r.f.name
	return nil
}

func TestNameServiceReturnsDisplayName(t *testing.T) {
	svc := NewNameService(nameFakeDB{name: "Acme Corp"})
	got, err := svc.Name(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("Name: %v", err)
	}
	if got != "Acme Corp" {
		t.Fatalf("name = %q, want Acme Corp", got)
	}
}

func TestNameServiceMapsNoRowsToNotFound(t *testing.T) {
	svc := NewNameService(nameFakeDB{err: pgx.ErrNoRows})
	_, err := svc.Name(context.Background(), "missing")
	if !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
}
