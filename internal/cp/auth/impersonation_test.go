package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/cp/audit"
)

// captureAudit records events written through the impersonation audit seam.
type captureAudit struct{ events []audit.Event }

func (c *captureAudit) fn() AuditFunc {
	return func(_ context.Context, e audit.Event) error {
		c.events = append(c.events, e)
		return nil
	}
}

// impDB is a fake for the impersonation service: it queues QueryRow results
// (the target-user platform-admin lookup and the insert RETURNING) and records
// every Exec (the audit insert, the End update) so tests can assert the writes.
type impDB struct {
	rows  []fakeRow
	execs []string
	tag   *fakeTag
}

func (f *impDB) Exec(_ context.Context, sql string, _ ...any) (pgconnTag, error) {
	f.execs = append(f.execs, sql)
	if f.tag != nil {
		return *f.tag, nil
	}
	return fakeTag{n: 1}, nil
}

func (f *impDB) QueryRow(_ context.Context, _ string, _ ...any) Row {
	if len(f.rows) == 0 {
		return fakeRow{err: errors.New("impDB: no queued row")}
	}
	r := f.rows[0]
	f.rows = f.rows[1:]
	return r
}

func TestImpersonationStartRejectsMissingArgs(t *testing.T) {
	aud := &captureAudit{}
	svc := &ImpersonationService{DB: &impDB{}, Now: fixedClock(time.Now()), Audit: aud.fn()}
	cases := []struct {
		name             string
		admin, ten, targ string
	}{
		{"no admin", "", "t-1", "u-2"},
		{"no tenant", "u-1", "", "u-2"},
		{"no target", "u-1", "t-1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Start(context.Background(), c.admin, c.ten, c.targ)
			if err == nil {
				t.Fatalf("want error for %s, got nil", c.name)
			}
		})
	}
}

func TestImpersonationStartWritesGrantAndAudit(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	db := &impDB{rows: []fakeRow{
		{vals: []any{"imp-1"}}, // insert ... returning id
	}}
	aud := &captureAudit{}
	svc := &ImpersonationService{DB: db, Now: fixedClock(now), Audit: aud.fn()}

	grant, err := svc.Start(context.Background(), "admin-1", "tenant-9", "member-3")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if grant.ID != "imp-1" {
		t.Fatalf("grant id = %q, want imp-1", grant.ID)
	}
	if grant.AdminUserID != "admin-1" || grant.TenantID != "tenant-9" || grant.ImpersonatedUserID != "member-3" {
		t.Fatalf("grant does not carry both identities: %+v", grant)
	}
	// Time-bounded: expires exactly the default window ahead.
	if !grant.ExpiresAt.Equal(now.Add(defaultImpersonationTTL)) {
		t.Fatalf("expires = %v, want %v", grant.ExpiresAt, now.Add(defaultImpersonationTTL))
	}
	// An admin.impersonate audit event carrying both identities and the
	// impersonation flag must have been written (SPEC-02 §4/§6).
	if len(aud.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(aud.events))
	}
	e := aud.events[0]
	if e.Action != "admin.impersonate" {
		t.Fatalf("audit action = %q, want admin.impersonate", e.Action)
	}
	if e.ActorUserID == nil || *e.ActorUserID != "admin-1" {
		t.Fatalf("audit actor = %v, want the real admin admin-1", e.ActorUserID)
	}
	if e.TenantID == nil || *e.TenantID != "tenant-9" {
		t.Fatalf("audit tenant = %v, want tenant-9", e.TenantID)
	}
	if e.TargetID == nil || *e.TargetID != "member-3" {
		t.Fatalf("audit target = %v, want member-3", e.TargetID)
	}
	if v, _ := e.Details["impersonation"].(bool); !v {
		t.Fatalf("audit details.impersonation != true: %+v", e.Details)
	}
}

func TestImpersonationEndStampsAndAudits(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// End first reads the grant to attribute the audit, then updates, then audits.
	db := &impDB{rows: []fakeRow{
		{vals: []any{"admin-1", "tenant-9", "member-3"}}, // select for the grant being ended
	}}
	aud := &captureAudit{}
	svc := &ImpersonationService{DB: db, Now: fixedClock(now), Audit: aud.fn()}

	if err := svc.End(context.Background(), "imp-1", "admin-1"); err != nil {
		t.Fatalf("End: %v", err)
	}
	var wroteUpdate bool
	for _, e := range db.execs {
		if strings.Contains(e, "update impersonation_sessions") {
			wroteUpdate = true
		}
	}
	if !wroteUpdate {
		t.Fatalf("End did not update the grant; execs=%v", db.execs)
	}
	if len(aud.events) != 1 || aud.events[0].Action != "admin.impersonate.end" {
		t.Fatalf("End did not write an admin.impersonate.end audit event; got %+v", aud.events)
	}
}

func TestImpersonationEndUnknownGrantFailsClosed(t *testing.T) {
	db := &impDB{rows: []fakeRow{{err: errNoRows{}}}}
	aud := &captureAudit{}
	svc := &ImpersonationService{DB: db, Now: fixedClock(time.Now()), Audit: aud.fn()}
	err := svc.End(context.Background(), "nope", "admin-1")
	if !errors.Is(err, ErrNoImpersonation) {
		t.Fatalf("want ErrNoImpersonation, got %v", err)
	}
}

// TestActiveImpersonationExpiryFailsClosed proves the "active" predicate refuses
// an expired or ended grant.
func TestActiveImpersonationExpiryFailsClosed(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		grant  Impersonation
		wantOK bool
	}{
		{"live", Impersonation{ExpiresAt: now.Add(time.Hour)}, true},
		{"expired", Impersonation{ExpiresAt: now.Add(-time.Minute)}, false},
		{"ended", Impersonation{ExpiresAt: now.Add(time.Hour), EndedAt: ptrTime(now.Add(-time.Minute))}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.grant.Active(now); got != c.wantOK {
				t.Fatalf("Active = %v, want %v", got, c.wantOK)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
