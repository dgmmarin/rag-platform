package tenant

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestWithTenantIDRoundTrips(t *testing.T) {
	id := ID(uuid.New())
	ctx := WithTenantID(context.Background(), id)

	got, ok := TenantIDFromCtx(ctx)
	if !ok {
		t.Fatalf("TenantIDFromCtx: want ok, got !ok")
	}
	if got != id {
		t.Fatalf("TenantIDFromCtx: want %s, got %s", id, got)
	}
}

func TestTenantIDFromCtxAbsent(t *testing.T) {
	if _, ok := TenantIDFromCtx(context.Background()); ok {
		t.Fatalf("TenantIDFromCtx on empty context: want !ok")
	}
}

func TestIDString(t *testing.T) {
	u := uuid.New()
	if ID(u).String() != u.String() {
		t.Fatalf("ID.String() = %q, want %q", ID(u).String(), u.String())
	}
}
