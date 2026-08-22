package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// API-key authentication errors. They are internal decision signals: the HTTP
// layer collapses every failure to 401 (or 403 for a scope miss) so a caller
// cannot distinguish "unknown", "revoked", or "tampered" and enumerate keys.
var (
	// ErrInvalidKey covers a malformed value, an unknown prefix, a hash
	// mismatch (tampered/wrong secret), and a revoked key — all indistinguishable
	// to the caller (no enumeration oracle), mirroring ErrNoSession.
	ErrInvalidKey = errors.New("auth: invalid api key")
	// ErrKeyExpired is a live, correct key whose expiry has passed. It maps to
	// the same 401 as ErrInvalidKey at the HTTP layer.
	ErrKeyExpired = errors.New("auth: api key expired")
)

// lastUsedThrottle is the minimum interval between last_used_at writes for a key
// (SPEC-09 §3: "updated at most once per minute"), avoiding a write per request.
const lastUsedThrottle = time.Minute

// KeyPrincipal is the authenticated identity behind a presented API key: the
// tenant the key belongs to (FR-ACC-03 — derived from the credential, never a
// client parameter) and the scopes it may exercise.
type KeyPrincipal struct {
	KeyID    string
	TenantID string
	Scopes   ScopeSet
}

// verifierDB is the database surface the verifier needs. *pgxpool.Pool satisfies
// it via poolDB, so the same wrapper serves the service and this verifier.
type verifierDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Exec(ctx context.Context, sql string, args ...any) (pgconnTag, error)
}

// APIKeyVerifier authenticates a presented `Authorization: Bearer rk_...` value
// against control-plane api_keys (SPEC-02 §3): it splits the value, looks the
// row up by its clear prefix, constant-time-matches the sha256 of the full
// secret, and rejects revoked or expired keys. A successful verify resolves the
// tenant and scopes and stamps last_used_at (throttled). It is stateless and
// safe for concurrent use.
type APIKeyVerifier struct {
	DB  verifierDB
	Now Clock
}

// NewAPIKeyVerifier builds a verifier over the given DB.
func NewAPIKeyVerifier(db verifierDB) *APIKeyVerifier {
	return &APIKeyVerifier{DB: db, Now: time.Now}
}

func (v *APIKeyVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// parseKey splits rk_<prefix>_<secret> into its prefix (for lookup) and the full
// presented value (for hashing). The secret body is base64url and may contain
// '_', so it is parsed with SplitN(_, 3), never a plain Split.
func parseKey(presented string) (prefix string, ok bool) {
	parts := strings.SplitN(presented, "_", 3)
	if len(parts) != 3 || parts[0] != apiKeyScheme || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return parts[1], true
}

// Verify authenticates a presented key value and returns its principal. Lookup
// filters revoked keys in SQL (a revoked key is immediately indistinguishable
// from unknown); expiry is checked in Go so it can be honoured explicitly. On
// success it advances last_used_at at most once per minute (SPEC-09 §3).
func (v *APIKeyVerifier) Verify(ctx context.Context, presented string) (KeyPrincipal, error) {
	prefix, ok := parseKey(presented)
	if !ok {
		return KeyPrincipal{}, ErrInvalidKey
	}
	now := v.now()

	var (
		id, tenantID string
		scopeStrs    []string
		expiresAt    *time.Time
		lastUsedAt   *time.Time
	)
	err := v.DB.QueryRow(ctx,
		`select id::text, tenant_id::text, scopes, expires_at, last_used_at
		 from api_keys
		 where key_prefix = $1 and key_hash = $2 and revoked_at is null`,
		prefix, hashToken(presented)).Scan(&id, &tenantID, &scopeStrs, &expiresAt, &lastUsedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return KeyPrincipal{}, ErrInvalidKey
		}
		return KeyPrincipal{}, fmt.Errorf("auth: look up api key: %w", err)
	}

	if expiresAt != nil && !now.Before(*expiresAt) {
		return KeyPrincipal{}, ErrKeyExpired
	}

	scopes, err := ParseScopes(scopeStrs)
	if err != nil {
		// A stored scope outside the spec set fails closed rather than granting.
		return KeyPrincipal{}, ErrInvalidKey
	}

	v.touchLastUsed(ctx, id, lastUsedAt, now)
	return KeyPrincipal{KeyID: id, TenantID: tenantID, Scopes: scopes}, nil
}

// touchLastUsed stamps last_used_at at most once per minute. A failed write is
// non-fatal to the request (last-used is a convenience field, not an auth gate)
// and is swallowed so a stats write never blocks an authenticated call.
func (v *APIKeyVerifier) touchLastUsed(ctx context.Context, id string, lastUsed *time.Time, now time.Time) {
	if lastUsed != nil && now.Sub(*lastUsed) < lastUsedThrottle {
		return
	}
	_, _ = v.DB.Exec(ctx, `update api_keys set last_used_at = $2 where id = $1`, id, now)
}

// RequireScope returns middleware that authenticates the Bearer API key on the
// request and requires the given scope. It resolves the tenant from the key
// (FR-ACC-03) and injects the tenant identity into the request context for
// downstream handlers and observability. Responses: 401 for a missing or
// invalid/expired key; 403 when the key is valid but lacks the scope.
//
// STORY-04.1 attaches this to concrete routes; this story implements the
// middleware and verifier so 04.1 only wires them.
func (v *APIKeyVerifier) RequireScope(scope Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := bearerToken(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			p, err := v.Verify(r.Context(), presented)
			if err != nil {
				if errors.Is(err, ErrInvalidKey) || errors.Is(err, ErrKeyExpired) {
					writeError(w, http.StatusUnauthorized, "authentication required")
					return
				}
				writeError(w, http.StatusInternalServerError, "authentication failed")
				return
			}
			if !p.Scopes.Has(scope) {
				writeError(w, http.StatusForbidden, "insufficient scope")
				return
			}

			tid, err := uuid.Parse(p.TenantID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "authentication failed")
				return
			}
			ctx := tenant.WithTenantID(r.Context(), tenant.ID(tid))
			ctx = WithKeyID(ctx, p.KeyID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the credential from an `Authorization: Bearer <token>`
// header, case-insensitively on the scheme.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}
