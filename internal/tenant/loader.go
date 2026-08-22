package tenant

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// controlPlaneLoader reads a tenant's status and connection details from the
// control plane (tenants + tenant_databases) — never tenant content (C-3). A
// tenant with no tenant_databases row (still provisioning) resolves to a record
// with empty connection details; the resolver refuses it on status alone.
func controlPlaneLoader(control *pgxpool.Pool) loaderFunc {
	return func(ctx context.Context, id ID) (Record, error) {
		const q = `
			select t.status,
			       coalesce(d.host, ''), coalesce(d.port, 0),
			       coalesce(d.database_name, ''), coalesce(d.username, ''),
			       d.password_enc,
			       coalesce(d.ssl_mode, 'require'),
			       coalesce(d.schema_version, 0),
			       coalesce(d.max_conns, 5)
			from tenants t
			left join tenant_databases d on d.tenant_id = t.id
			where t.id = $1`

		var (
			rec         = Record{ID: id}
			status      string
			passwordEnc []byte
		)
		err := control.QueryRow(ctx, q, [16]byte(id)).Scan(
			&status, &rec.Host, &rec.Port, &rec.Database, &rec.Username,
			&passwordEnc, &rec.SSLMode, &rec.SchemaVersion, &rec.MaxConns,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return Record{}, ErrTenantNotFound
		}
		if err != nil {
			return Record{}, fmt.Errorf("tenant registry: load %s: %w", id, err)
		}
		rec.Status = Status(status)
		rec.PasswordEnc = passwordEnc
		return rec, nil
	}
}

// passwordDecryptingBuilder returns a poolBuilder that decrypts the tenant
// password (SPEC-09 §2) and dials the tenant's dedicated database with its
// per-tenant role (SPEC-01 §4, SPEC-09 isolation). The plaintext password lives
// only in the DSN passed to pgxpool and is never logged.
func passwordDecryptingBuilder(dec Decrypter) poolBuilder {
	return func(ctx context.Context, rec Record) (*pgxpool.Pool, error) {
		if dec == nil {
			return nil, fmt.Errorf("tenant %s: no decrypter configured", rec.ID)
		}
		password, err := dec.Decrypt(rec.PasswordEnc)
		if err != nil {
			return nil, fmt.Errorf("tenant %s: decrypt password: %w", rec.ID, err)
		}

		cfg, err := pgxpool.ParseConfig(dsn(rec, string(password)))
		// Zero the plaintext copy we hold as soon as the DSN is parsed.
		for i := range password {
			password[i] = 0
		}
		if err != nil {
			return nil, fmt.Errorf("tenant %s: parse pool config: %w", rec.ID, err)
		}
		if rec.MaxConns > 0 {
			cfg.MaxConns = int32(rec.MaxConns)
		}
		cfg.MinConns = 0

		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("tenant %s: build pool: %w", rec.ID, err)
		}
		return pool, nil
	}
}

// dsn assembles a Postgres connection string for a tenant record. The password
// is URL-escaped; the caller passes it separately so it is never held on Record.
func dsn(rec Record, password string) string {
	ssl := rec.SSLMode
	if ssl == "" {
		ssl = "require"
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(rec.Username, password),
		Host:   fmt.Sprintf("%s:%d", rec.Host, rec.Port),
		Path:   "/" + rec.Database,
	}
	q := u.Query()
	q.Set("sslmode", ssl)
	u.RawQuery = q.Encode()
	return u.String()
}
