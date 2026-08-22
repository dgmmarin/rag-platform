package tenant

import (
	"testing"

	"github.com/google/uuid"
)

func TestHandleNotificationInvalidatesTenant(t *testing.T) {
	id := ID(uuid.New())
	spy := &spyRegistry{}

	if err := handleNotification(spy, id.String()); err != nil {
		t.Fatalf("handleNotification: %v", err)
	}
	if len(spy.invalidated) != 1 || spy.invalidated[0] != id {
		t.Fatalf("invalidated = %v, want [%s]", spy.invalidated, id)
	}
}

func TestHandleNotificationRejectsBadPayload(t *testing.T) {
	spy := &spyRegistry{}
	if err := handleNotification(spy, "not-a-uuid"); err == nil {
		t.Fatal("handleNotification with bad payload: want error, got nil")
	}
	if len(spy.invalidated) != 0 {
		t.Fatalf("invalidated on bad payload = %v, want none", spy.invalidated)
	}
}

// TenantChannel is the documented channel name; assert it matches the migration.
func TestTenantChannelName(t *testing.T) {
	if TenantChangedChannel != "tenant_changed" {
		t.Fatalf("channel = %q, want tenant_changed (SPEC-01 §3)", TenantChangedChannel)
	}
}
