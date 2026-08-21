// Package migrate applies database migrations for the RAG platform (STORY-01.5,
// SPEC-02 §7). Control-plane migrations live in control/*.sql as goose scripts;
// they are embedded so `ragctl migrate control` needs no files on disk.
//
// The human-readable schemas/control_plane.sql stays the documented shape of the
// control plane; ControlSchemaSQL reconstructs the applied DDL from the goose Up
// bodies so a drift test can prove the two never disagree.
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx stdlib driver for database/sql
)

//go:embed control/*.sql
var controlMigrations embed.FS

// Control opens the control-plane database at dbURL and applies the embedded
// goose migrations. It is idempotent: goose records applied versions in
// goose_db_version, so a rerun is a no-op.
func Control(ctx context.Context, dbURL string) error {
	if strings.TrimSpace(dbURL) == "" {
		return fmt.Errorf("migrate control: empty database URL")
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("migrate control: open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(controlMigrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("migrate control: set dialect: %w", err)
	}

	if err := goose.UpContext(ctx, db, "control"); err != nil {
		return fmt.Errorf("migrate control: up: %w", err)
	}
	return nil
}

// gooseDirective matches goose annotation lines (`-- +goose ...`), which are
// directives, not SQL, and so must be stripped before comparing against the
// hand-written schema file.
var gooseDirective = regexp.MustCompile(`(?m)^\s*--\s*\+goose.*$`)

// ControlSchemaSQL reconstructs the DDL the control migrations apply by
// concatenating every migration's Up section (goose directive lines removed),
// in filename order. This is the "what the migrations create" side of the drift
// guard against schemas/control_plane.sql.
func ControlSchemaSQL() (string, error) {
	entries, err := fs.Glob(controlMigrations, "control/*.sql")
	if err != nil {
		return "", fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(entries)

	var b strings.Builder
	for _, name := range entries {
		raw, err := controlMigrations.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", name, err)
		}
		up, err := upSection(string(raw))
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		b.WriteString(gooseDirective.ReplaceAllString(up, ""))
		b.WriteString("\n")
	}
	return b.String(), nil
}

// upSection returns the text between the `-- +goose Up` and `-- +goose Down`
// markers of a goose migration.
func upSection(migration string) (string, error) {
	const upMarker = "-- +goose Up"
	const downMarker = "-- +goose Down"

	up := strings.Index(migration, upMarker)
	if up < 0 {
		return "", fmt.Errorf("missing %q marker", upMarker)
	}
	body := migration[up+len(upMarker):]
	if down := strings.Index(body, downMarker); down >= 0 {
		body = body[:down]
	}
	return body, nil
}

// normalizeSQL collapses insignificant whitespace so the drift comparison is
// robust to reindentation: leading/trailing space is trimmed and every run of
// whitespace becomes a single space. It does not strip comments — a change in an
// explanatory comment is still real drift worth flagging.
var whitespace = regexp.MustCompile(`\s+`)

func normalizeSQL(s string) string {
	return strings.TrimSpace(whitespace.ReplaceAllString(s, " "))
}
