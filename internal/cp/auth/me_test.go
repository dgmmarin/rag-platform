package auth

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// meRow is one tenant_members/tenants join row used by fakeMeDB's Query.
type meRow struct {
	tenantID, slug, name, role string
}

// fakeMeRows is an in-memory Rows implementation over a fixed set of
// membership rows, mimicking pgx.Rows for MeService.Me's Query call.
type fakeMeRows struct {
	rows []meRow
	i    int
}

func (r *fakeMeRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *fakeMeRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	*dest[0].(*string) = row.tenantID
	*dest[1].(*string) = row.slug
	*dest[2].(*string) = row.name
	*dest[3].(*string) = row.role
	return nil
}

func (r *fakeMeRows) Err() error { return nil }
func (r *fakeMeRows) Close()     {}

// fakeMeDB implements MembershipDB for MeService tests: a fixed user row and
// membership rows, keyed off which of the two queries Me issues.
type fakeMeDB struct {
	email           string
	isPlatformAdmin bool
	memberships     []meRow
}

func newFakeMeDB(t *testing.T) *fakeMeDB {
	t.Helper()
	return &fakeMeDB{
		email:           "a@b.com",
		isPlatformAdmin: true,
		memberships:     []meRow{{tenantID: "t1", slug: "acme", name: "Acme Inc", role: "admin"}},
	}
}

func (d *fakeMeDB) Exec(_ context.Context, _ string, _ ...any) (pgconnTag, error) {
	return fakeTag{n: 0}, nil
}

func (d *fakeMeDB) QueryRow(_ context.Context, _ string, _ ...any) Row {
	return fakeRow{vals: []any{d.email, d.isPlatformAdmin}}
}

func (d *fakeMeDB) Query(_ context.Context, _ string, _ ...any) (Rows, error) {
	return &fakeMeRows{rows: d.memberships}, nil
}

func TestMeServiceReturnsUserAdminFlagAndMemberships(t *testing.T) {
	db := newFakeMeDB(t) // user {id:"u1",email:"a@b.com",is_platform_admin:true} + 1 membership {acme,admin}
	got, err := NewMeService(db).Me(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got.User.Email != "a@b.com" || !got.IsPlatformAdmin {
		t.Fatalf("user/admin: %+v", got)
	}
	if len(got.Memberships) != 1 || got.Memberships[0].Slug != "acme" || string(got.Memberships[0].Role) != "admin" {
		t.Fatalf("memberships: %+v", got.Memberships)
	}
}

func TestMeHandler200WithSessionAnd401Without(t *testing.T) {
	h := &MeHandlers{Service: NewMeService(newFakeMeDB(t))}
	ctx := ContextWithSession(context.Background(), Session{UserID: "u1", CSRFToken: "csrf-x"})
	req := httptest.NewRequest("GET", "/v1/auth/me", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.Me(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"csrf_token":"csrf-x"`) {
		t.Fatalf("with session: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.Me(rec, httptest.NewRequest("GET", "/v1/auth/me", nil))
	if rec.Code != 401 {
		t.Fatalf("no session want 401, got %d", rec.Code)
	}
}
