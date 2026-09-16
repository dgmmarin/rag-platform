package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// keyRec is one in-memory api_keys row for the handler tests.
type keyRec struct {
	id, name, prefix string
	scopes           []string
	createdAt        time.Time
	revoked          bool
}

// fakeKeysDB implements MembershipDB for the api-key handler tests: it stores the
// minted rows (never a secret) so List/Create/Revoke branch mapping is exercised
// without Postgres.
type fakeKeysDB struct {
	rows map[string]*keyRec // id -> row
	seq  int
}

func newFakeKeysDB() *fakeKeysDB { return &fakeKeysDB{rows: map[string]*keyRec{}} }

func (d *fakeKeysDB) Exec(_ context.Context, sql string, args ...any) (pgconnTag, error) {
	if strings.Contains(sql, "update api_keys set revoked_at") {
		id := args[0].(string)
		row, ok := d.rows[id]
		if !ok || row.revoked {
			return fakeTag{n: 0}, nil
		}
		row.revoked = true
		return fakeTag{n: 1}, nil
	}
	return fakeTag{n: 0}, nil
}

func (d *fakeKeysDB) QueryRow(_ context.Context, sql string, args ...any) Row {
	switch {
	case strings.Contains(sql, "insert into api_keys"):
		d.seq++
		id := "key-" + string(rune('0'+d.seq))
		// args: tenant_id, name, key_hash, key_prefix, scopes, created_by, created_at, expires_at
		d.rows[id] = &keyRec{
			id:        id,
			name:      args[1].(string),
			prefix:    args[3].(string),
			scopes:    args[4].([]string),
			createdAt: args[6].(time.Time),
		}
		return fakeRow{vals: []any{id}}
	case strings.Contains(sql, "select exists"):
		id := args[0].(string)
		_, ok := d.rows[id]
		return fakeRow{vals: []any{ok}}
	}
	return fakeRow{err: errNoRows{}}
}

func (d *fakeKeysDB) Query(_ context.Context, sql string, _ ...any) (Rows, error) {
	if !strings.Contains(sql, "from api_keys") {
		return nil, errNoRows{}
	}
	var out []*keyRec
	for _, r := range d.rows {
		out = append(out, r)
	}
	return &fakeKeyRows{rows: out}, nil
}

type fakeKeyRows struct {
	rows []*keyRec
	i    int
}

func (r *fakeKeyRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *fakeKeyRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	// id, name, key_prefix, scopes, created_at, expires_at, revoked_at, last_used_at
	*dest[0].(*string) = row.id
	*dest[1].(*string) = row.name
	*dest[2].(*string) = row.prefix
	*dest[3].(*[]string) = row.scopes
	*dest[4].(*time.Time) = row.createdAt
	*dest[5].(**time.Time) = nil
	if row.revoked {
		now := time.Now()
		*dest[6].(**time.Time) = &now
	} else {
		*dest[6].(**time.Time) = nil
	}
	*dest[7].(**time.Time) = nil
	return nil
}

func (r *fakeKeyRows) Err() error { return nil }
func (r *fakeKeyRows) Close()     {}

const keyTenant = "33333333-3333-3333-3333-333333333333"

func keyReq(method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	ctx := tenant.WithTenantID(r.Context(), tenant.ID(uuid.MustParse(keyTenant)))
	ctx = ContextWithSession(ctx, Session{UserID: "admin-1"})
	return r.WithContext(ctx)
}

func newKeyHandlers() (*APIKeyHandlers, *fakeKeysDB) {
	db := newFakeKeysDB()
	return NewAPIKeyHandlers(&APIKeyService{DB: db, Now: time.Now}), db
}

func TestKeyCreateReturnsSecretOnce(t *testing.T) {
	h, _ := newKeyHandlers()
	rr := httptest.NewRecorder()
	h.Create(rr, keyReq(http.MethodPost, "/api-keys", `{"name":"ci","scopes":["query"]}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Key    string         `json:"key"`
		Record map[string]any `json:"record"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if got.Key == "" {
		t.Fatal("create did not return the plaintext secret")
	}
	if got.Record["name"] != "ci" {
		t.Fatalf("record = %v", got.Record)
	}
	// The record must never carry the secret.
	if _, present := got.Record["key"]; present {
		t.Fatal("record leaked the secret")
	}
}

func TestKeyCreateUnknownScope400(t *testing.T) {
	h, _ := newKeyHandlers()
	rr := httptest.NewRecorder()
	h.Create(rr, keyReq(http.MethodPost, "/api-keys", `{"name":"ci","scopes":["nope"]}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestKeyCreateBadExpiry400(t *testing.T) {
	h, _ := newKeyHandlers()
	rr := httptest.NewRecorder()
	h.Create(rr, keyReq(http.MethodPost, "/api-keys", `{"name":"ci","scopes":["query"],"expires_at":"not-a-date"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestKeyListHasNoSecret(t *testing.T) {
	h, db := newKeyHandlers()
	db.rows["key-1"] = &keyRec{id: "key-1", name: "ci", prefix: "rk_ci", scopes: []string{"query"}, createdAt: time.Now()}
	rr := httptest.NewRecorder()
	h.List(rr, keyReq(http.MethodGet, "/api-keys", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(strings.ToLower(rr.Body.String()), "\"key\"") {
		t.Fatalf("list leaked a secret field; body=%s", rr.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0]["name"] != "ci" {
		t.Fatalf("list = %v", got)
	}
}

func TestKeyRevoke(t *testing.T) {
	h, db := newKeyHandlers()
	db.rows["key-1"] = &keyRec{id: "key-1", name: "ci", prefix: "rk_ci", scopes: []string{"query"}, createdAt: time.Now()}
	rr := httptest.NewRecorder()
	r := keyReq(http.MethodDelete, "/api-keys/key-1", "")
	r.SetPathValue("keyId", "key-1")
	h.Revoke(rr, r)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if !db.rows["key-1"].revoked {
		t.Fatal("key not revoked")
	}
}

func TestKeyRevokeUnknown404(t *testing.T) {
	h, _ := newKeyHandlers()
	rr := httptest.NewRecorder()
	r := keyReq(http.MethodDelete, "/api-keys/ghost", "")
	r.SetPathValue("keyId", "ghost")
	h.Revoke(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}
