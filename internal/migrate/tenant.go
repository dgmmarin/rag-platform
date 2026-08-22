package migrate

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx stdlib driver for database/sql
)

//go:embed tenant/*.sql
var tenantMigrations embed.FS

// dimensionPlaceholder is the token in the tenant migrations standing in for the
// per-tenant embedding vector dimension. The runner substitutes it with the
// tenant's tenants.settings.embedding_dim at apply time (SPEC-01 §6/§7): one
// migration set serves tenants on different embedding models.
const dimensionPlaceholder = "EMBEDDING_DIM"

// defaultEmbeddingDim is the documented default dimension (matches
// schemas/tenant.sql). It is used by the drift test and as the fallback when a
// tenant has not pinned settings.embedding_dim.
const defaultEmbeddingDim = 1536

// ExpectedTenantVersion is the highest embedded tenant migration version. The
// resolver imports this to fail closed against any tenant behind it (SPEC-01
// §7); deriving it from the files (not a hand-kept constant) means it cannot
// silently drift from what `migrate tenants` applies.
func ExpectedTenantVersion() (int64, error) {
	entries, err := fs.Glob(tenantMigrations, "tenant/*.sql")
	if err != nil {
		return 0, fmt.Errorf("glob tenant migrations: %w", err)
	}
	var highest int64
	for _, name := range entries {
		v, err := gooseFileVersion(name)
		if err != nil {
			return 0, err
		}
		if v > highest {
			highest = v
		}
	}
	if highest == 0 {
		return 0, errors.New("no tenant migrations embedded")
	}
	return highest, nil
}

// gooseFileVersion parses the leading integer from a goose migration filename
// (e.g. tenant/00001_initial_schema.sql -> 1).
func gooseFileVersion(name string) (int64, error) {
	base := name
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	num := base
	if i := strings.IndexByte(base, '_'); i >= 0 {
		num = base[:i]
	}
	v, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migration %q: cannot parse version: %w", name, err)
	}
	return v, nil
}

// TenantSchemaSQL reconstructs the DDL the tenant migrations apply at the given
// embedding dimension by concatenating every migration's Up section (goose
// directives removed), in filename order. This is the "what the migrations
// create" side of the drift guard against schemas/tenant.sql.
func TenantSchemaSQL(dim int) (string, error) {
	entries, err := fs.Glob(tenantMigrations, "tenant/*.sql")
	if err != nil {
		return "", fmt.Errorf("glob tenant migrations: %w", err)
	}
	sort.Strings(entries)

	var b strings.Builder
	for _, name := range entries {
		raw, err := tenantMigrations.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", name, err)
		}
		up, err := upSection(string(raw))
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		up = gooseDirective.ReplaceAllString(up, "")
		b.WriteString(substituteDimension(up, dim))
		b.WriteString("\n")
	}
	return b.String(), nil
}

// substituteDimension replaces the embedding-dimension placeholder with a
// concrete positive integer.
func substituteDimension(sql string, dim int) string {
	return strings.ReplaceAll(sql, dimensionPlaceholder, strconv.Itoa(dim))
}

// substituteDimensionFS returns an fs.FS with the same tenant/*.sql tree as src
// but the embedding-dimension placeholder replaced by dim. goose reads
// migrations from an fs.FS; giving it a per-tenant substituted view keeps goose
// the single migration engine while realising the placeholder (SPEC-01 §6/§7).
// A non-positive dimension is rejected so a mis-provisioned tenant fails closed
// rather than applying a broken schema.
func substituteDimensionFS(src fs.FS, dim int) (fs.FS, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("embedding dimension must be positive, got %d", dim)
	}
	entries, err := fs.Glob(src, "tenant/*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob tenant migrations: %w", err)
	}
	out := memFS{}
	for _, name := range entries {
		raw, err := fs.ReadFile(src, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		out[name] = []byte(substituteDimension(string(raw), dim))
	}
	return out, nil
}

// TenantResult reports the outcome of a `migrate tenants` run so the CLI can
// print a summary and set a non-zero exit code listing failed tenants
// (SPEC-01 §7).
type TenantResult struct {
	// Migrated is the set of tenants that advanced (or were already current).
	Migrated []TenantOutcome
	// Failed is the set of tenants whose migration errored; the run continues
	// past them (SPEC-01 §7) and a rerun resumes only these.
	Failed []TenantOutcome
}

// TenantOutcome is a single tenant's result.
type TenantOutcome struct {
	TenantID string
	Slug     string
	Version  int64 // schema version after the run (best known)
	Err      error
}

// HasFailures reports whether any tenant failed.
func (r TenantResult) HasFailures() bool { return len(r.Failed) > 0 }

// tenantRow is a control-plane registry row the runner iterates over.
type tenantRow struct {
	id       string
	slug     string
	host     string
	port     int
	database string
	username string
	password string // decrypted
	sslMode  string
	dim      int
}

// PasswordDecrypter opens an envelope-encrypted tenant password (SPEC-09 §2).
// *crypto.Cipher satisfies it; migrate depends on the behaviour, not the type.
type PasswordDecrypter interface {
	Decrypt(ciphertext []byte) ([]byte, error)
}

// TenantOptions configures a `migrate tenants` run.
type TenantOptions struct {
	// Parallel bounds how many tenants migrate concurrently (SPEC-01 §7: default 4).
	Parallel int
	// Decrypter unwraps each tenant's stored password to dial its database.
	Decrypter PasswordDecrypter
	// OnlyTenant, if set, restricts the run to the tenant with this slug.
	OnlyTenant string
}

// Tenants applies pending tenant migrations to every eligible tenant in
// parallel, records the applied version in tenant_databases.schema_version, and
// continues past per-tenant failures (SPEC-01 §7, FR-TEN-09). Each tenant's row
// is locked FOR UPDATE SKIP LOCKED so concurrent runners (or another running
// migration) never collide, which is what makes a rerun a safe, resumable
// operation. It returns the per-tenant outcomes; the caller sets the process
// exit code from Result.HasFailures.
func Tenants(ctx context.Context, controlURL string, opts TenantOptions) (TenantResult, error) {
	if strings.TrimSpace(controlURL) == "" {
		return TenantResult{}, fmt.Errorf("migrate tenants: empty control-plane URL")
	}
	if opts.Decrypter == nil {
		return TenantResult{}, fmt.Errorf("migrate tenants: no decrypter configured")
	}
	parallel := opts.Parallel
	if parallel <= 0 {
		parallel = 4
	}

	control, err := pgxpool.New(ctx, controlURL)
	if err != nil {
		return TenantResult{}, fmt.Errorf("migrate tenants: connect control plane: %w", err)
	}
	defer control.Close()

	rows, err := loadMigratableTenants(ctx, control, opts)
	if err != nil {
		return TenantResult{}, err
	}

	return runTenantMigrations(ctx, control, rows, parallel), nil
}

// loadMigratableTenants reads active/suspended/provisioning tenants that have a
// tenant_databases row, decrypting each password. Deleted/deleting tenants are
// skipped: there is nothing to migrate (SPEC-01 §7/§8).
func loadMigratableTenants(ctx context.Context, control *pgxpool.Pool, opts TenantOptions) ([]tenantRow, error) {
	const q = `
		select t.id::text, t.slug, d.host, d.port, d.database_name, d.username,
		       d.password_enc, coalesce(d.ssl_mode, 'require'),
		       coalesce((t.settings->>'embedding_dim')::int, $1)
		from tenants t
		join tenant_databases d on d.tenant_id = t.id
		where t.status in ('provisioning', 'active', 'suspended')
		  and ($2 = '' or t.slug = $2)
		order by t.slug`

	pgRows, err := control.Query(ctx, q, defaultEmbeddingDim, opts.OnlyTenant)
	if err != nil {
		return nil, fmt.Errorf("migrate tenants: load registry: %w", err)
	}
	defer pgRows.Close()

	var out []tenantRow
	for pgRows.Next() {
		var (
			r           tenantRow
			passwordEnc []byte
		)
		if err := pgRows.Scan(&r.id, &r.slug, &r.host, &r.port, &r.database,
			&r.username, &passwordEnc, &r.sslMode, &r.dim); err != nil {
			return nil, fmt.Errorf("migrate tenants: scan registry row: %w", err)
		}
		password, err := opts.Decrypter.Decrypt(passwordEnc)
		if err != nil {
			return nil, fmt.Errorf("migrate tenants: decrypt password for %s: %w", r.slug, err)
		}
		r.password = string(password)
		for i := range password {
			password[i] = 0
		}
		out = append(out, r)
	}
	if err := pgRows.Err(); err != nil {
		return nil, fmt.Errorf("migrate tenants: iterate registry: %w", err)
	}
	return out, nil
}

// runTenantMigrations fans out over tenants with a bounded worker pool, applying
// each tenant's migrations and mirroring the resulting version to the control
// plane. Failures are collected, not fatal (SPEC-01 §7).
func runTenantMigrations(ctx context.Context, control *pgxpool.Pool, rows []tenantRow, parallel int) TenantResult {
	var (
		mu     sync.Mutex
		result TenantResult
		wg     sync.WaitGroup
		sem    = make(chan struct{}, parallel)
	)

	for _, row := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func(row tenantRow) {
			defer wg.Done()
			defer func() { <-sem }()

			version, err := migrateOneTenant(ctx, control, row)
			outcome := TenantOutcome{TenantID: row.id, Slug: row.slug, Version: version, Err: err}
			mu.Lock()
			if err != nil {
				result.Failed = append(result.Failed, outcome)
			} else {
				result.Migrated = append(result.Migrated, outcome)
			}
			mu.Unlock()
		}(row)
	}
	wg.Wait()

	sort.Slice(result.Migrated, func(i, j int) bool { return result.Migrated[i].Slug < result.Migrated[j].Slug })
	sort.Slice(result.Failed, func(i, j int) bool { return result.Failed[i].Slug < result.Failed[j].Slug })
	return result
}

// migrateOneTenant locks the tenant's registry row (FOR UPDATE SKIP LOCKED),
// applies pending goose migrations against its dedicated database with the
// per-tenant embedding dimension substituted, and mirrors the applied version
// into schema_version — all in one control-plane transaction so a crash cannot
// leave schema_version claiming a version the tenant DB never reached. If
// another runner already holds the row lock, this tenant is skipped (not
// failed): the other runner owns it.
func migrateOneTenant(ctx context.Context, control *pgxpool.Pool, row tenantRow) (version int64, err error) {
	tx, err := control.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the row; SKIP LOCKED means a concurrent runner working this tenant
	// makes us see no row and skip cleanly.
	var locked string
	lockErr := tx.QueryRow(ctx,
		`select tenant_id::text from tenant_databases
		 where tenant_id = $1 for update skip locked`, row.id).Scan(&locked)
	if errors.Is(lockErr, pgx.ErrNoRows) {
		return 0, nil // held by another runner; resumable rerun will catch it
	}
	if lockErr != nil {
		return 0, fmt.Errorf("lock row: %w", lockErr)
	}

	version, err = applyTenantMigrations(ctx, row)
	if err != nil {
		return 0, err
	}

	if _, err = tx.Exec(ctx,
		`update tenant_databases set schema_version = $2, updated_at = now()
		 where tenant_id = $1`, row.id, version); err != nil {
		return version, fmt.Errorf("record schema_version: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return version, fmt.Errorf("commit: %w", err)
	}
	return version, nil
}

// applyTenantMigrations opens the tenant's database with its per-tenant role and
// runs goose Up from an FS whose embedding-dimension placeholder is substituted
// for this tenant. Returns the tenant DB's current goose version after Up.
func applyTenantMigrations(ctx context.Context, row tenantRow) (int64, error) {
	subFS, err := substituteDimensionFS(tenantMigrations, row.dim)
	if err != nil {
		return 0, fmt.Errorf("tenant %s: %w", row.slug, err)
	}
	rooted, err := fs.Sub(subFS, "tenant")
	if err != nil {
		return 0, fmt.Errorf("tenant %s: sub fs: %w", row.slug, err)
	}

	db, err := sql.Open("pgx", tenantDSN(row))
	if err != nil {
		return 0, fmt.Errorf("tenant %s: open db: %w", row.slug, err)
	}
	defer func() { _ = db.Close() }()

	// A fresh Provider per tenant avoids goose's global dialect/baseFS state,
	// which is not safe for the parallel fan-out.
	provider, err := goose.NewProvider(goose.DialectPostgres, db, rooted)
	if err != nil {
		return 0, fmt.Errorf("tenant %s: goose provider: %w", row.slug, err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return 0, fmt.Errorf("tenant %s: apply migrations: %w", row.slug, err)
	}
	current, err := provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("tenant %s: read applied version: %w", row.slug, err)
	}
	return current, nil
}

// tenantDSN assembles the tenant database connection string. The password is
// URL-escaped and never logged.
func tenantDSN(row tenantRow) string {
	ssl := row.sslMode
	if ssl == "" {
		ssl = "require"
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(row.username, row.password),
		Host:   fmt.Sprintf("%s:%d", row.host, row.port),
		Path:   "/" + row.database,
	}
	q := u.Query()
	q.Set("sslmode", ssl)
	q.Set("connect_timeout", strconv.Itoa(int((15 * time.Second).Seconds())))
	u.RawQuery = q.Encode()
	return u.String()
}
