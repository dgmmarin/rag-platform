package auth

import (
	"context"
	"testing"
)

// The key-id context helper round-trips and reports absence.
func TestKeyIDContextRoundTrip(t *testing.T) {
	if _, ok := KeyIDFromCtx(context.Background()); ok {
		t.Fatal("empty context should carry no key id")
	}
	ctx := WithKeyID(context.Background(), "key-123")
	got, ok := KeyIDFromCtx(ctx)
	if !ok || got != "key-123" {
		t.Fatalf("KeyIDFromCtx = %q,%v; want key-123,true", got, ok)
	}
}
