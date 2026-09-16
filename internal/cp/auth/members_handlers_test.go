package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// memberRec is one in-memory membership row (email + role) for the handler tests.
type memberRec struct {
	email string
	role  Role
}

// fakeMembersDB implements MembershipDB for the membership-handler tests: a users
// table (email -> id) plus tenant_members rows, keyed the same way the service's
// SQL scopes them. It covers UserByEmail, AddMember, ListMembers, SetMemberRole
// and RemoveMember, including the last-owner guard, so the branch mapping to HTTP
// statuses is exercised without Postgres (the SQL round trip is proven e2e).
type fakeMembersDB struct {
	users   map[string]string               // email -> user id
	members map[string]map[string]memberRec // tenantID -> userID -> rec
}

func newFakeMembersDB() *fakeMembersDB {
	return &fakeMembersDB{
		users:   map[string]string{},
		members: map[string]map[string]memberRec{},
	}
}

func (d *fakeMembersDB) tenant(t string) map[string]memberRec {
	if d.members[t] == nil {
		d.members[t] = map[string]memberRec{}
	}
	return d.members[t]
}

func (d *fakeMembersDB) ownerCount(t string) int {
	n := 0
	for _, r := range d.tenant(t) {
		if r.role == RoleOwner {
			n++
		}
	}
	return n
}

func (d *fakeMembersDB) Exec(_ context.Context, sql string, args ...any) (pgconnTag, error) {
	switch {
	case strings.Contains(sql, "update tenant_members set role"):
		tid, uid, newRole := args[0].(string), args[1].(string), args[2].(string)
		rec, ok := d.tenant(tid)[uid]
		if !ok {
			return fakeTag{n: 0}, nil
		}
		if rec.role == RoleOwner && Role(newRole) != RoleOwner && d.ownerCount(tid) <= 1 {
			return fakeTag{n: 0}, nil
		}
		rec.role = Role(newRole)
		d.tenant(tid)[uid] = rec
		return fakeTag{n: 1}, nil
	case strings.Contains(sql, "delete from tenant_members"):
		tid, uid := args[0].(string), args[1].(string)
		rec, ok := d.tenant(tid)[uid]
		if !ok {
			return fakeTag{n: 0}, nil
		}
		if rec.role == RoleOwner && d.ownerCount(tid) <= 1 {
			return fakeTag{n: 0}, nil
		}
		delete(d.tenant(tid), uid)
		return fakeTag{n: 1}, nil
	}
	return fakeTag{n: 0}, nil
}

func (d *fakeMembersDB) QueryRow(_ context.Context, sql string, args ...any) Row {
	switch {
	case strings.Contains(sql, "from users where email"):
		id, ok := d.users[args[0].(string)]
		if !ok {
			return fakeRow{err: errNoRows{}}
		}
		return fakeRow{vals: []any{id}}
	case strings.Contains(sql, "insert into tenant_members"):
		tid, uid, role := args[0].(string), args[1].(string), args[2].(string)
		if _, exists := d.tenant(tid)[uid]; exists {
			return fakeRow{err: uniqueErr{}}
		}
		d.tenant(tid)[uid] = memberRec{email: d.emailFor(uid), role: Role(role)}
		return fakeRow{vals: []any{true}}
	case strings.Contains(sql, "select exists"):
		tid, uid := args[0].(string), args[1].(string)
		_, exists := d.tenant(tid)[uid]
		return fakeRow{vals: []any{exists}}
	}
	return fakeRow{err: errNoRows{}}
}

func (d *fakeMembersDB) emailFor(uid string) string {
	for e, id := range d.users {
		if id == uid {
			return e
		}
	}
	return ""
}

func (d *fakeMembersDB) Query(_ context.Context, sql string, args ...any) (Rows, error) {
	if !strings.Contains(sql, "from tenant_members") {
		return nil, errNoRows{}
	}
	tid := args[0].(string)
	var rows []memberListRow
	for uid, rec := range d.tenant(tid) {
		rows = append(rows, memberListRow{tid, uid, rec.email, string(rec.role)})
	}
	return &fakeMemberRows{rows: rows}, nil
}

type memberListRow struct{ tenantID, userID, email, role string }

type fakeMemberRows struct {
	rows []memberListRow
	i    int
}

func (r *fakeMemberRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *fakeMemberRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	*dest[0].(*string) = row.tenantID
	*dest[1].(*string) = row.userID
	*dest[2].(*string) = row.email
	*dest[3].(*string) = row.role
	return nil
}

func (r *fakeMemberRows) Err() error { return nil }
func (r *fakeMemberRows) Close()     {}

const memberTenant = "22222222-2222-2222-2222-222222222222"

func memberReq(method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	ctx := tenant.WithTenantID(r.Context(), tenant.ID(uuid.MustParse(memberTenant)))
	ctx = ContextWithSession(ctx, Session{UserID: "admin-1"})
	return r.WithContext(ctx)
}

func newMemberHandlers() (*MembershipHandlers, *fakeMembersDB) {
	db := newFakeMembersDB()
	return NewMembershipHandlers(&MembershipService{DB: db}), db
}

func TestMemberListReturnsRoster(t *testing.T) {
	h, db := newMemberHandlers()
	db.users["a@b.com"] = "u1"
	db.tenant(memberTenant)["u1"] = memberRec{email: "a@b.com", role: RoleEditor}

	rr := httptest.NewRecorder()
	h.List(rr, memberReq(http.MethodGet, "/members", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if len(got) != 1 || got[0]["email"] != "a@b.com" || got[0]["role"] != "editor" {
		t.Fatalf("roster = %v", got)
	}
}

func TestMemberAddResolvesEmail(t *testing.T) {
	h, db := newMemberHandlers()
	db.users["new@b.com"] = "u2"

	rr := httptest.NewRecorder()
	h.Add(rr, memberReq(http.MethodPost, "/members", `{"email":"new@b.com","role":"editor"}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := db.tenant(memberTenant)["u2"]; !ok {
		t.Fatal("member not added")
	}
}

func TestMemberAddUnknownEmail404(t *testing.T) {
	h, _ := newMemberHandlers()
	rr := httptest.NewRecorder()
	h.Add(rr, memberReq(http.MethodPost, "/members", `{"email":"ghost@b.com","role":"editor"}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMemberAddBadRole400(t *testing.T) {
	h, db := newMemberHandlers()
	db.users["new@b.com"] = "u2"
	rr := httptest.NewRecorder()
	h.Add(rr, memberReq(http.MethodPost, "/members", `{"email":"new@b.com","role":"superadmin"}`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMemberAddDuplicate409(t *testing.T) {
	h, db := newMemberHandlers()
	db.users["a@b.com"] = "u1"
	db.tenant(memberTenant)["u1"] = memberRec{email: "a@b.com", role: RoleViewer}
	rr := httptest.NewRecorder()
	h.Add(rr, memberReq(http.MethodPost, "/members", `{"email":"a@b.com","role":"editor"}`))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMemberSetRole(t *testing.T) {
	h, db := newMemberHandlers()
	db.users["a@b.com"] = "u1"
	db.tenant(memberTenant)["u1"] = memberRec{email: "a@b.com", role: RoleViewer}
	rr := httptest.NewRecorder()
	r := memberReq(http.MethodPatch, "/members/u1", `{"role":"editor"}`)
	r.SetPathValue("userId", "u1")
	h.SetRole(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if db.tenant(memberTenant)["u1"].role != RoleEditor {
		t.Fatalf("role = %q, want editor", db.tenant(memberTenant)["u1"].role)
	}
}

func TestMemberSetRoleLastOwner409(t *testing.T) {
	h, db := newMemberHandlers()
	db.users["o@b.com"] = "owner1"
	db.tenant(memberTenant)["owner1"] = memberRec{email: "o@b.com", role: RoleOwner}
	rr := httptest.NewRecorder()
	r := memberReq(http.MethodPatch, "/members/owner1", `{"role":"admin"}`)
	r.SetPathValue("userId", "owner1")
	h.SetRole(rr, r)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMemberRemove(t *testing.T) {
	h, db := newMemberHandlers()
	db.users["o@b.com"] = "owner1"
	db.users["e@b.com"] = "ed1"
	db.tenant(memberTenant)["owner1"] = memberRec{email: "o@b.com", role: RoleOwner}
	db.tenant(memberTenant)["ed1"] = memberRec{email: "e@b.com", role: RoleEditor}
	rr := httptest.NewRecorder()
	r := memberReq(http.MethodDelete, "/members/ed1", "")
	r.SetPathValue("userId", "ed1")
	h.Remove(rr, r)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if _, ok := db.tenant(memberTenant)["ed1"]; ok {
		t.Fatal("member not removed")
	}
}

func TestMemberRemoveLastOwner409(t *testing.T) {
	h, db := newMemberHandlers()
	db.users["o@b.com"] = "owner1"
	db.tenant(memberTenant)["owner1"] = memberRec{email: "o@b.com", role: RoleOwner}
	rr := httptest.NewRecorder()
	r := memberReq(http.MethodDelete, "/members/owner1", "")
	r.SetPathValue("userId", "owner1")
	h.Remove(rr, r)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}
