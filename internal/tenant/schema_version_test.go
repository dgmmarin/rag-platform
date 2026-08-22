package tenant

import (
	"testing"

	"github.com/rag-platform/ragctl/internal/migrate"
)

// TestExpectedSchemaVersionMatchesMigrations proves the resolver's fail-closed
// version (SPEC-01 §7) is derived from the embedded tenant migrations, not a
// hand-maintained constant that could silently drift from what `ragctl migrate
// tenants` actually applies (STORY-02.2). If a new migration is added, this
// keeps Open fail-closed against tenants that have not yet been migrated to it.
func TestExpectedSchemaVersionMatchesMigrations(t *testing.T) {
	want, err := migrate.ExpectedTenantVersion()
	if err != nil {
		t.Fatalf("ExpectedTenantVersion: %v", err)
	}
	if int64(expectedSchemaVersion) != want {
		t.Fatalf("expectedSchemaVersion = %d, want %d (highest embedded tenant migration)",
			expectedSchemaVersion, want)
	}
}
