package migrate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// repoFile reads a file relative to the repository root regardless of the
// working directory `go test` was invoked from.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller path")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestControlSchemaMatchesMigrations is the STORY-01.5 drift guard: the
// human-readable schemas/control_plane.sql must stay byte-for-byte (modulo
// whitespace) equal to what the goose migrations actually apply, so the schema
// file never lies about the deployed shape of the control plane.
func TestControlSchemaMatchesMigrations(t *testing.T) {
	schema := repoFile(t, "schemas/control_plane.sql")

	migrated, err := ControlSchemaSQL()
	if err != nil {
		t.Fatalf("ControlSchemaSQL: %v", err)
	}

	if got, want := normalizeSQL(migrated), normalizeSQL(schema); got != want {
		t.Fatalf("control migrations drifted from schemas/control_plane.sql\n"+
			"regenerate one from the other.\n--- migrations ---\n%s\n--- schema ---\n%s", got, want)
	}
}
