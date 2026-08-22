package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeExec records the last insert so a test can assert what Record wrote.
type fakeExec struct {
	sql  string
	args []any
	err  error
}

func (f *fakeExec) Exec(_ context.Context, sql string, args ...any) (commandTag, error) {
	f.sql = sql
	f.args = args
	return fakeTag{1}, f.err
}

type fakeTag struct{ n int64 }

func (t fakeTag) RowsAffected() int64 { return t.n }

func strptr(s string) *string { return &s }

func TestRecordInsertsRowWithActorTenantTarget(t *testing.T) {
	db := &fakeExec{}
	tenant := "11111111-1111-1111-1111-111111111111"
	actor := "22222222-2222-2222-2222-222222222222"
	target := tenant
	err := Record(context.Background(), db, Event{
		TenantID:    &tenant,
		ActorUserID: &actor,
		Action:      "settings.update",
		TargetType:  strptr("tenant"),
		TargetID:    &target,
		Details:     map[string]any{"changed": []string{"chunking"}},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !strings.Contains(strings.ToLower(db.sql), "insert into audit_log") {
		t.Fatalf("expected insert into audit_log, got: %s", db.sql)
	}
	// action must be carried through as an arg
	found := false
	for _, a := range db.args {
		if s, ok := a.(string); ok && s == "settings.update" {
			found = true
		}
	}
	if !found {
		t.Fatalf("action not passed as arg: %#v", db.args)
	}
}

func TestRecordRejectsEmptyAction(t *testing.T) {
	db := &fakeExec{}
	if err := Record(context.Background(), db, Event{Action: ""}); err == nil {
		t.Fatal("expected error for empty action, got nil")
	}
	if db.sql != "" {
		t.Fatalf("nothing should be written for an invalid event, wrote: %s", db.sql)
	}
}

func TestRecordDefaultsNilDetailsToEmptyObject(t *testing.T) {
	db := &fakeExec{}
	if err := Record(context.Background(), db, Event{Action: "apikey.revoke"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// details arg must be valid JSON object bytes, defaulting to {}
	var detailsArg []byte
	for _, a := range db.args {
		if b, ok := a.([]byte); ok {
			detailsArg = b
		}
	}
	if detailsArg == nil {
		t.Fatalf("no details []byte arg found: %#v", db.args)
	}
	var m map[string]any
	if err := json.Unmarshal(detailsArg, &m); err != nil {
		t.Fatalf("details not valid JSON object: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("expected empty details object, got %#v", m)
	}
}

func TestRecordPropagatesDBError(t *testing.T) {
	db := &fakeExec{err: errors.New("boom")}
	if err := Record(context.Background(), db, Event{Action: "member.add"}); err == nil {
		t.Fatal("expected DB error to propagate")
	}
}
