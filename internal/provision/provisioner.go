// Package provision implements tenant provisioning (STORY-02.3, FR-TEN-01/02,
// SPEC-01 §6): create a dedicated least-privilege role + database for a tenant on
// the target Postgres, install the superuser-only extensions the tenant schema
// assumes, encrypt and record the connection details in the control plane, apply
// the tenant migrations, and set the tenant active.
//
// The privileged (superuser) connection is used ONLY for the CREATE ROLE /
// CREATE DATABASE / CREATE EXTENSION steps and the control-plane registry
// writes; tenant migrations run as the per-tenant least-privilege role via the
// STORY-02.2 runner (internal/migrate). Passwords are envelope-encrypted with the
// same Cipher the resolver decrypts with (SPEC-09 §2, C-4) and are never logged.
//
// Provisioning is idempotent: re-running for an existing tenant reuses its stored
// connection details (so the encrypted password keeps matching the live role),
// ensures the role/database/extensions exist, re-applies pending migrations, and
// re-sets the tenant active. This makes it safe to retry as a job (ADR-0005) and
// safe to invoke synchronously from `ragctl enroll` until the River wiring lands
// in EPIC-09.
package provision

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/migrate"
)

// errValidation tags parameter/precondition failures so callers (and tests) can
// distinguish a bad request from a downstream infrastructure error.
var errValidation = errors.New("provision: invalid parameters")

// defaultEmbeddingDim mirrors the tenant schema default (schemas/tenant.sql);
// used when Params.EmbeddingDim is zero.
const defaultEmbeddingDim = 1536

// Encrypter seals a plaintext secret (SPEC-09 §2). *crypto.Cipher satisfies it;
// provision depends on the behaviour, not the concrete type. The ciphertext must
// be openable by the resolver's Decrypter, so the two must share a DEK/version.
type Encrypter interface {
	Encrypt(plaintext []byte) ([]byte, error)
}

// Decrypter opens an envelope-encrypted secret; the tenant migration runner needs
// it to dial each tenant's database. *crypto.Cipher satisfies it.
type Decrypter interface {
	Decrypt(ciphertext []byte) ([]byte, error)
}

// Params describes the tenant to provision. The tenant ID is assigned by the
// control plane on insert (or reused if the slug already exists).
type Params struct {
	Slug   string // url-safe tenant slug (unique); required
	Name   string // display name; required
	Region string // data-residency hint; optional (schema default applies)

	// Host/Port/SSLMode describe where the tenant database lives and how the
	// resolver (a host process) will reach it. Host/Port default from the
	// privileged connection's target if left zero.
	Host    string
	Port    int
	SSLMode string

	// EmbeddingDim pins the tenant's vector dimension (tenants.settings.embedding_dim).
	// Zero uses the schema default (1536); negative is rejected.
	EmbeddingDim int

	// MaxConns caps the per-tenant pool; zero uses the schema default.
	MaxConns int
}

// Result reports what was provisioned. It never carries the plaintext password.
type Result struct {
	TenantID      string
	Slug          string
	DatabaseName  string
	Role          string
	Host          string
	Port          int
	SchemaVersion int64
	Created       bool // true if this call created the tenant row (vs. a retry)
}

// Provisioner creates and records tenants. It holds no per-request state, so a
// single value is safe to reuse.
type Provisioner struct {
	// PrivilegedURL is the superuser connection to the Postgres cluster's admin
	// database (typically the control-plane URL). Used for CREATE ROLE/DATABASE/
	// EXTENSION and the control-plane registry writes.
	PrivilegedURL string
	// Encrypter seals the generated tenant password (SPEC-09 §2).
	Encrypter Encrypter
	// Decrypter unwraps stored passwords for the migration runner; usually the
	// same Cipher as Encrypter.
	Decrypter Decrypter
	// TenantHost/TenantPort override the host:port recorded for the tenant when
	// Params leaves them zero (e.g. the cluster is reached by a service name from
	// the app but a mapped port from tests).
	TenantHost string
	TenantPort int
}

// validate checks required fields and normalises defaults, returning a copy of
// params with defaults applied.
func (p *Provisioner) validate(params Params) (Params, error) {
	if p.Encrypter == nil {
		return Params{}, fmt.Errorf("%w: no encrypter configured", errValidation)
	}
	if strings.TrimSpace(p.PrivilegedURL) == "" {
		return Params{}, fmt.Errorf("%w: no privileged connection URL", errValidation)
	}
	if strings.TrimSpace(params.Slug) == "" {
		return Params{}, fmt.Errorf("%w: slug is required", errValidation)
	}
	if strings.TrimSpace(params.Name) == "" {
		return Params{}, fmt.Errorf("%w: name is required", errValidation)
	}
	if params.EmbeddingDim < 0 {
		return Params{}, fmt.Errorf("%w: embedding dimension must be positive", errValidation)
	}
	if params.EmbeddingDim == 0 {
		params.EmbeddingDim = defaultEmbeddingDim
	}
	if params.Host == "" {
		params.Host = p.TenantHost
	}
	if params.Port == 0 {
		params.Port = p.TenantPort
	}
	if params.SSLMode == "" {
		params.SSLMode = "require"
	}
	return params, nil
}

// Provision provisions the tenant described by params and returns the result. It
// is idempotent: re-running for an existing slug reuses the recorded connection
// details, ensures the role/database/extensions exist, re-applies migrations, and
// re-sets the tenant active.
func (p *Provisioner) Provision(ctx context.Context, params Params) (Result, error) {
	params, err := p.validate(params)
	if err != nil {
		return Result{}, err
	}

	admin, err := pgxpool.New(ctx, p.PrivilegedURL)
	if err != nil {
		return Result{}, fmt.Errorf("provision: connect privileged: %w", err)
	}
	defer admin.Close()
	// Fail fast on an unreachable cluster so callers get a connection error, not
	// a hang; the pool created above is lazy.
	if err := admin.Ping(ctx); err != nil {
		return Result{}, fmt.Errorf("provision: reach privileged connection: %w", err)
	}

	// 1) Ensure the tenants registry row (status provisioning). Idempotent by slug.
	reg, created, err := ensureTenantRow(ctx, admin, params)
	if err != nil {
		return Result{}, err
	}

	// 2) Reuse recorded connection details on retry; otherwise derive fresh ones,
	//    generate + encrypt a password, and record them.
	dbRow, err := ensureTenantDatabaseRow(ctx, admin, p.Encrypter, reg, params)
	if err != nil {
		return Result{}, err
	}

	// 3) Create the role + database (idempotent) and install the extensions.
	if err := createRoleAndDatabase(ctx, admin, dbRow); err != nil {
		return Result{}, err
	}
	if err := installExtensions(ctx, p.PrivilegedURL, dbRow); err != nil {
		return Result{}, err
	}

	// 4) Apply tenant migrations via the STORY-02.2 runner (runs as the
	//    per-tenant role). Requires a decrypter to open the stored password.
	if p.Decrypter == nil {
		return Result{}, fmt.Errorf("%w: no decrypter configured for migrations", errValidation)
	}
	migRes, err := migrate.Tenants(ctx, p.PrivilegedURL, migrate.TenantOptions{
		Parallel:   1,
		Decrypter:  p.Decrypter,
		OnlyTenant: reg.slug,
	})
	if err != nil {
		return Result{}, fmt.Errorf("provision: run tenant migrations: %w", err)
	}
	if migRes.HasFailures() {
		return Result{}, fmt.Errorf("provision: tenant migration failed: %v", migRes.Failed[0].Err)
	}
	var version int64
	for _, o := range migRes.Migrated {
		if o.Slug == reg.slug {
			version = o.Version
		}
	}

	// 5) Set status active and write an audit event, in one transaction.
	if err := activateAndAudit(ctx, admin, reg, dbRow, created); err != nil {
		return Result{}, err
	}

	return Result{
		TenantID:      reg.id,
		Slug:          reg.slug,
		DatabaseName:  dbRow.database,
		Role:          dbRow.username,
		Host:          dbRow.host,
		Port:          dbRow.port,
		SchemaVersion: version,
		Created:       created,
	}, nil
}

// registryRow is the control-plane tenants row.
type registryRow struct {
	id   string
	slug string
}

// databaseRow is the control-plane tenant_databases row plus the plaintext
// password needed transiently to create the role. The plaintext is zeroed by the
// creator once the role exists; it is never stored on Result or logged.
type databaseRow struct {
	tenantID string
	host     string
	port     int
	database string
	username string
	sslMode  string
	dim      int
	maxConns int
	password string // plaintext; empty on retry (role already exists)
}

// ensureTenantRow inserts a provisioning tenants row for the slug, or reuses the
// existing one on retry. It flips an existing tenant back to provisioning so a
// retry after a partial failure has consistent status until activation.
func ensureTenantRow(ctx context.Context, admin *pgxpool.Pool, params Params) (registryRow, bool, error) {
	var reg registryRow
	err := admin.QueryRow(ctx,
		`select id::text, slug from tenants where slug = $1`, params.Slug).Scan(&reg.id, &reg.slug)
	if err == nil {
		if _, uerr := admin.Exec(ctx,
			`update tenants set status = 'provisioning', name = $2,
			 region = coalesce(nullif($3,''), region),
			 settings = jsonb_set(coalesce(settings,'{}'), '{embedding_dim}', to_jsonb($4::int))
			 where id = $1`, reg.id, params.Name, params.Region, params.EmbeddingDim); uerr != nil {
			return registryRow{}, false, fmt.Errorf("provision: reset tenant to provisioning: %w", uerr)
		}
		return reg, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return registryRow{}, false, fmt.Errorf("provision: look up tenant: %w", err)
	}

	region := params.Region
	insert := `insert into tenants (slug, name, status, settings`
	values := `values ($1, $2, 'provisioning', jsonb_build_object('embedding_dim', $3::int)`
	args := []any{params.Slug, params.Name, params.EmbeddingDim}
	if region != "" {
		insert += `, region`
		values += `, $4`
		args = append(args, region)
	}
	insert += `) ` + values + `) returning id::text, slug`
	if err := admin.QueryRow(ctx, insert, args...).Scan(&reg.id, &reg.slug); err != nil {
		return registryRow{}, false, fmt.Errorf("provision: insert tenant: %w", err)
	}
	return reg, true, nil
}

// ensureTenantDatabaseRow reuses an existing tenant_databases row (retry) or
// derives fresh names, generates + encrypts a password, and inserts one.
func ensureTenantDatabaseRow(ctx context.Context, admin *pgxpool.Pool, enc Encrypter, reg registryRow, params Params) (databaseRow, error) {
	var row databaseRow
	err := admin.QueryRow(ctx,
		`select host, port, database_name, username, ssl_mode, max_conns
		 from tenant_databases where tenant_id = $1`, reg.id).Scan(
		&row.host, &row.port, &row.database, &row.username, &row.sslMode, &row.maxConns)
	if err == nil {
		row.tenantID = reg.id
		row.dim = params.EmbeddingDim
		// password left empty: role already exists, do not regenerate.
		return row, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return databaseRow{}, fmt.Errorf("provision: look up tenant_databases: %w", err)
	}

	// Fresh provisioning: derive names from the slug + a short suffix from the ID.
	suffix := strings.ReplaceAll(reg.id, "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	dbName, role := deriveNames(params.Slug, suffix)
	password, err := generatePassword()
	if err != nil {
		return databaseRow{}, err
	}
	encPassword, err := enc.Encrypt([]byte(password))
	if err != nil {
		return databaseRow{}, fmt.Errorf("provision: encrypt password: %w", err)
	}

	maxConns := params.MaxConns
	row = databaseRow{
		tenantID: reg.id,
		host:     params.Host,
		port:     params.Port,
		database: dbName,
		username: role,
		sslMode:  params.SSLMode,
		dim:      params.EmbeddingDim,
		maxConns: maxConns,
		password: password,
	}

	insert := `insert into tenant_databases
		(tenant_id, host, port, database_name, username, password_enc, ssl_mode, schema_version)
		values ($1, $2, $3, $4, $5, $6, $7, 0)`
	if _, err := admin.Exec(ctx, insert,
		reg.id, row.host, row.port, row.database, row.username, encPassword, row.sslMode); err != nil {
		return databaseRow{}, fmt.Errorf("provision: insert tenant_databases: %w", err)
	}
	if maxConns > 0 {
		if _, err := admin.Exec(ctx,
			`update tenant_databases set max_conns = $2 where tenant_id = $1`, reg.id, maxConns); err != nil {
			return databaseRow{}, fmt.Errorf("provision: set max_conns: %w", err)
		}
	}
	return row, nil
}

// createRoleAndDatabase creates the least-privilege role and its owned database
// on the privileged connection, idempotently (skips whichever already exists).
func createRoleAndDatabase(ctx context.Context, admin *pgxpool.Pool, row databaseRow) error {
	// Role: create only if absent. On retry (empty password) the role already
	// exists, so we never attempt to recreate it.
	if row.password != "" {
		exists, err := roleExists(ctx, admin, row.username)
		if err != nil {
			return err
		}
		if !exists {
			stmt, err := createRoleSQL(row.username, row.password)
			if err != nil {
				return err
			}
			if _, err := admin.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("provision: create role: %w", err)
			}
		}
	}

	// Database: CREATE DATABASE cannot run inside a transaction and has no IF NOT
	// EXISTS, so guard with an existence check.
	dbExists, err := databaseExists(ctx, admin, row.database)
	if err != nil {
		return err
	}
	if !dbExists {
		stmt, err := createDatabaseSQL(row.database, row.username)
		if err != nil {
			return err
		}
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("provision: create database: %w", err)
		}
	}

	// Close the connection-level boundary (NFR-SEC-01, SPEC-01 §9): revoke the
	// default PUBLIC CONNECT so no other tenant's role can address this database,
	// and grant CONNECT back to the owner. Idempotent, so retries are safe and it
	// also repairs databases created before this lockdown existed.
	lockdown, err := lockdownDatabaseSQL(row.database, row.username)
	if err != nil {
		return err
	}
	for _, stmt := range lockdown {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("provision: lock down database connect privilege: %w", err)
		}
	}
	return nil
}

// roleExists reports whether a login role of the given name exists.
func roleExists(ctx context.Context, admin *pgxpool.Pool, role string) (bool, error) {
	var one int
	err := admin.QueryRow(ctx, `select 1 from pg_roles where rolname = $1`, role).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("provision: check role: %w", err)
	}
	return true, nil
}

// databaseExists reports whether a database of the given name exists.
func databaseExists(ctx context.Context, admin *pgxpool.Pool, name string) (bool, error) {
	var one int
	err := admin.QueryRow(ctx, `select 1 from pg_database where datname = $1`, name).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("provision: check database: %w", err)
	}
	return true, nil
}

// installExtensions connects (as the privileged user) to the newly created
// tenant database and installs the required extensions. This must happen before
// the tenant migrations, which assume the extensions exist (SPEC-01 §6).
func installExtensions(ctx context.Context, privilegedURL string, row databaseRow) error {
	adminDBURL, err := adminURLForDatabase(privilegedURL, row.database)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, adminDBURL)
	if err != nil {
		return fmt.Errorf("provision: connect tenant db as superuser: %w", err)
	}
	defer pool.Close()
	for _, stmt := range extensionStatements() {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("provision: install extension: %w", err)
		}
	}
	return nil
}

// activateAndAudit sets the tenant active and records an audit event in one
// control-plane transaction (C-3: control-plane-only, no tenant content).
func activateAndAudit(ctx context.Context, admin *pgxpool.Pool, reg registryRow, row databaseRow, created bool) error {
	tx, err := admin.Begin(ctx)
	if err != nil {
		return fmt.Errorf("provision: begin activation tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`update tenants set status = 'active', updated_at = now() where id = $1`, reg.id); err != nil {
		return fmt.Errorf("provision: set active: %w", err)
	}

	action := "tenant.provision"
	if created {
		action = "tenant.create"
	}
	// details carry connection metadata only — never the password (SPEC-09 §2).
	if _, err := tx.Exec(ctx,
		`insert into audit_log (tenant_id, action, target_type, target_id, details)
		 values ($1, $2, 'tenant', $1, jsonb_build_object(
			'slug', $3::text, 'database', $4::text, 'role', $5::text, 'host', $6::text))`,
		reg.id, action, reg.slug, row.database, row.username, row.host); err != nil {
		return fmt.Errorf("provision: write audit event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("provision: commit activation: %w", err)
	}
	return nil
}

// adminURLForDatabase rewrites the privileged connection URL to target a
// different database (the newly created tenant DB) while keeping the same
// superuser credentials, host and options. Used to install extensions inside the
// tenant database as the privileged user.
func adminURLForDatabase(privilegedURL, database string) (string, error) {
	u, err := url.Parse(privilegedURL)
	if err != nil {
		return "", fmt.Errorf("provision: parse privileged URL: %w", err)
	}
	u.Path = "/" + database
	return u.String(), nil
}
