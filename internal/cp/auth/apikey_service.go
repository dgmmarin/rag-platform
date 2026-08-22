package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrKeyNotFound is returned when a revoke targets a key that does not exist for
// the tenant, letting a handler answer 404 rather than silently succeeding.
var ErrKeyNotFound = errors.New("auth: api key not found")

// APIKeyRecord is a stored api_keys row for display (FR-ACC-04 list). It never
// carries the plaintext secret — only the clear prefix, scopes, and lifecycle
// timestamps. The full secret exists only in the Create return value, once.
type APIKeyRecord struct {
	ID         string
	Name       string
	Prefix     string
	Scopes     []string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// CreateKeyParams are the inputs to minting a key. TenantID is resolved from the
// authenticated admin's tenant (FR-ACC-03), never a client parameter. CreatedBy
// is optional (informational). ExpiresAt is optional (FR-ACC-04 expiry).
type CreateKeyParams struct {
	TenantID  string
	Name      string
	Scopes    []string
	CreatedBy *string
	ExpiresAt *time.Time
}

// APIKeyService owns api_keys (SPEC-02 §2). It mints keys (returning the secret
// once), lists them for display, and revokes them. It is stateless and safe for
// concurrent use.
type APIKeyService struct {
	DB  MembershipDB
	Now Clock
}

// NewAPIKeyService builds a service over the given DB.
func NewAPIKeyService(db MembershipDB) *APIKeyService {
	return &APIKeyService{DB: db, Now: time.Now}
}

func (s *APIKeyService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Create mints a new API key for the tenant. It validates scopes against the
// spec set before any write, mints a secret, stores only its sha256 and clear
// prefix, and returns the stored record plus the plaintext secret — the ONLY
// time the secret is ever available (FR-ACC-05, C-4). The raw secret is never
// logged.
func (s *APIKeyService) Create(ctx context.Context, p CreateKeyParams) (APIKeyRecord, string, error) {
	if p.Name == "" {
		return APIKeyRecord{}, "", fmt.Errorf("auth: api key name required")
	}
	scopes, err := ParseScopes(p.Scopes)
	if err != nil {
		return APIKeyRecord{}, "", err
	}

	secret, prefix, hash, err := newAPIKeySecret()
	if err != nil {
		return APIKeyRecord{}, "", err
	}

	now := s.now()
	scopeSlice := scopes.Slice()
	var id string
	err = s.DB.QueryRow(ctx,
		`insert into api_keys (tenant_id, name, key_hash, key_prefix, scopes, created_by, created_at, expires_at)
		 values ($1, $2, $3, $4, $5, $6, $7, $8)
		 returning id::text`,
		p.TenantID, p.Name, hash, prefix, scopeSlice, p.CreatedBy, now, p.ExpiresAt).Scan(&id)
	if err != nil {
		return APIKeyRecord{}, "", fmt.Errorf("auth: insert api key: %w", err)
	}

	return APIKeyRecord{
		ID:        id,
		Name:      p.Name,
		Prefix:    prefix,
		Scopes:    scopeSlice,
		CreatedAt: now,
		ExpiresAt: p.ExpiresAt,
	}, secret, nil
}

// List returns the tenant's keys for display, newest first, with prefix, scopes,
// and last-used timestamp but never the secret (FR-ACC-04). Revoked keys remain
// listed with their revoked_at set so the history is visible.
func (s *APIKeyService) List(ctx context.Context, tenantID string) ([]APIKeyRecord, error) {
	rows, err := s.DB.Query(ctx,
		`select id::text, name, key_prefix, scopes, created_at, expires_at, revoked_at, last_used_at
		 from api_keys
		 where tenant_id = $1
		 order by created_at desc`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("auth: list api keys: %w", err)
	}
	defer rows.Close()

	var out []APIKeyRecord
	for rows.Next() {
		var r APIKeyRecord
		if err := rows.Scan(&r.ID, &r.Name, &r.Prefix, &r.Scopes,
			&r.CreatedAt, &r.ExpiresAt, &r.RevokedAt, &r.LastUsedAt); err != nil {
			return nil, fmt.Errorf("auth: scan api key: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: iterate api keys: %w", err)
	}
	return out, nil
}

// Revoke marks a key revoked, taking effect immediately: the verifier's lookup
// filters `revoked_at is null`, so the next request with the key is refused. It
// is idempotent — revoking an already-revoked key is a no-op success — but
// returns ErrKeyNotFound if no such key exists for the tenant, so a handler can
// distinguish a bad id from a repeat call. Scoping the update to tenant_id
// prevents one tenant revoking another's key.
func (s *APIKeyService) Revoke(ctx context.Context, tenantID, keyID string) error {
	// First attempt: revoke a live key.
	tag, err := s.DB.Exec(ctx,
		`update api_keys set revoked_at = $3
		 where id = $1 and tenant_id = $2 and revoked_at is null`,
		keyID, tenantID, s.now())
	if err != nil {
		return fmt.Errorf("auth: revoke api key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Zero rows: either already revoked (idempotent success) or unknown.
	var exists bool
	if err := s.DB.QueryRow(ctx,
		`select exists(select 1 from api_keys where id = $1 and tenant_id = $2)`,
		keyID, tenantID).Scan(&exists); err != nil {
		return fmt.Errorf("auth: check api key: %w", err)
	}
	if !exists {
		return ErrKeyNotFound
	}
	return nil
}
