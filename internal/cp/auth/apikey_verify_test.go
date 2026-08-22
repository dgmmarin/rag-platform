package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// keyRow is a stored api_keys row the fake verifier DB returns.
type keyRow struct {
	id        string
	tenantID  string
	keyHash   []byte
	scopes    []string
	expiresAt *time.Time
}

// fakeKeyDB faithfully models the real lookup
//
//	select id, tenant_id, scopes, expires_at, last_used_at
//	from api_keys where key_prefix = $1 and key_hash = $2 and revoked_at is null
//
// keyed by (prefix, hash): a wrong hash (tampered secret) or a prefix with no
// row both return pgx.ErrNoRows, exactly like the real query. touchCount records
// the last_used_at Exec so the throttle can be asserted.
type fakeKeyDB struct {
	byPrefix   map[string]keyRow
	touchCount int
}

func (f *fakeKeyDB) Exec(_ context.Context, _ string, _ ...any) (pgconnTag, error) {
	f.touchCount++
	return fakeTag{n: 1}, nil
}

func (f *fakeKeyDB) QueryRow(_ context.Context, _ string, args ...any) Row {
	prefix, _ := args[0].(string) // $1
	hash, _ := args[1].([]byte)   // $2
	row, ok := f.byPrefix[prefix]
	if !ok || !bytes.Equal(row.keyHash, hash) {
		return fakeRow{err: pgx.ErrNoRows}
	}
	return keyScanRow{row}
}

func (f *fakeKeyDB) Query(context.Context, string, ...any) (Rows, error) {
	return nil, errors.New("unused")
}

// keyScanRow scans a keyRow into the verifier's destinations
// (id, tenant_id, scopes, expires_at, last_used_at).
type keyScanRow struct{ r keyRow }

func (s keyScanRow) Scan(dest ...any) error {
	*dest[0].(*string) = s.r.id
	*dest[1].(*string) = s.r.tenantID
	*dest[2].(*[]string) = s.r.scopes
	if s.r.expiresAt == nil {
		*dest[3].(**time.Time) = nil
	} else {
		v := *s.r.expiresAt
		*dest[3].(**time.Time) = &v
	}
	*dest[4].(**time.Time) = nil // last_used_at unused by the decision
	return nil
}

func mkVerifier(rows map[string]keyRow, now time.Time) (*APIKeyVerifier, *fakeKeyDB) {
	db := &fakeKeyDB{byPrefix: rows}
	return &APIKeyVerifier{DB: db, Now: fixedClock(now)}, db
}

// mintInto mints a real secret and stores its row (correct hash) under its
// prefix, returning the plaintext to present.
func mintInto(t *testing.T, rows map[string]keyRow, tenantID string, scopes ...string) (secret, prefix string) {
	t.Helper()
	secret, prefix, hash, err := newAPIKeySecret()
	if err != nil {
		t.Fatal(err)
	}
	rows[prefix] = keyRow{id: "key-1", tenantID: tenantID, keyHash: hash, scopes: scopes}
	return secret, prefix
}

// TestVerifyRejectsMalformed proves a value that is not rk_<prefix>_<secret> is
// refused without a DB round trip.
func TestVerifyRejectsMalformed(t *testing.T) {
	v, _ := mkVerifier(map[string]keyRow{}, time.Now())
	for _, bad := range []string{"", "rk_", "rk_abc", "xx_abc_def", "abcdef"} {
		if _, err := v.Verify(context.Background(), bad); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Verify(%q) = %v, want ErrInvalidKey", bad, err)
		}
	}
}

// TestVerifyRejectsUnknownAndTampered proves a prefix with no row, and a valid
// prefix with a wrong secret (hash mismatch), both fail as ErrInvalidKey.
func TestVerifyRejectsUnknownAndTampered(t *testing.T) {
	rows := map[string]keyRow{}
	_, prefix := mintInto(t, rows, "t-1", "query")
	v, _ := mkVerifier(rows, time.Now())

	tampered := "rk_" + prefix + "_wrongsecretwrongsecretwrongsecret"
	if _, err := v.Verify(context.Background(), tampered); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("tampered secret: got %v, want ErrInvalidKey", err)
	}
	unknown := "rk_zzzzzzzz_somesecretsomesecretsomesecret"
	if _, err := v.Verify(context.Background(), unknown); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("unknown prefix: got %v, want ErrInvalidKey", err)
	}
}

// TestVerifyRejectsExpired proves an expired key is refused (expiry honoured).
func TestVerifyRejectsExpired(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	rows := map[string]keyRow{}
	secret, prefix := mintInto(t, rows, "t-1", "query")
	row := rows[prefix]
	row.expiresAt = &past
	rows[prefix] = row
	v, _ := mkVerifier(rows, now)
	if _, err := v.Verify(context.Background(), secret); !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("expired key: got %v, want ErrKeyExpired", err)
	}
}

// TestVerifyGoldenPathResolvesTenantAndScopes proves a live key resolves to its
// tenant and scope set and stamps last_used_at.
func TestVerifyGoldenPathResolvesTenantAndScopes(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	rows := map[string]keyRow{}
	secret, _ := mintInto(t, rows, "t-42", "query", "ingest")
	v, db := mkVerifier(rows, now)
	p, err := v.Verify(context.Background(), secret)
	if err != nil {
		t.Fatalf("Verify golden: %v", err)
	}
	if p.TenantID != "t-42" {
		t.Fatalf("tenant = %q, want t-42", p.TenantID)
	}
	if !p.Scopes.Has(ScopeQuery) || !p.Scopes.Has(ScopeIngest) || p.Scopes.Has(ScopeAdmin) {
		t.Fatalf("scopes = %v, want {query,ingest}", p.Scopes)
	}
	if db.touchCount != 1 {
		t.Fatalf("last_used_at touched %d times, want 1", db.touchCount)
	}
}

// TestRequireScopeAllowsAndRejects proves the middleware resolves the Bearer
// key, enforces the required scope, injects the tenant into context, and maps
// missing auth to 401 and out-of-scope to 403.
func TestRequireScopeAllowsAndRejects(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	const tid = "11111111-1111-1111-1111-111111111111"
	rows := map[string]keyRow{}
	secret, _ := mintInto(t, rows, tid, "query")
	v, _ := mkVerifier(rows, now)
	mw := v.RequireScope(ScopeQuery)

	// Allowed: query scope, and the resolved tenant reaches the inner handler.
	{
		var gotTenant string
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id, ok := tenant.TenantIDFromCtx(r.Context()); ok {
				gotTenant = id.String()
			}
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/query", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rr := httptest.NewRecorder()
		mw(inner).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("allowed request: got %d, want 200", rr.Code)
		}
		if gotTenant != tid {
			t.Fatalf("tenant in ctx = %q, want %q", gotTenant, tid)
		}
	}

	// Refused: a scope the key lacks -> 403, inner never runs.
	{
		called := false
		inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called = true })
		req := httptest.NewRequest(http.MethodPost, "/v1/sources", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rr := httptest.NewRecorder()
		v.RequireScope(ScopeAdmin)(inner).ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden || called {
			t.Fatalf("out-of-scope: got %d called=%v, want 403", rr.Code, called)
		}
	}

	// No/garbage Authorization -> 401.
	{
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
		req := httptest.NewRequest(http.MethodPost, "/v1/query", nil)
		rr := httptest.NewRecorder()
		mw(inner).ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("no auth: got %d, want 401", rr.Code)
		}
	}
}
