package provision

import (
	"errors"
	"testing"
	"time"
)

// TestAllowedStatusTransitions pins the SPEC-01 §8 / FR-TEN-04/05 status machine:
// active<->suspended, active/suspended->deleting, deleting->(active|suspended)
// (cancellation), deleting->deleted (final). Everything else is rejected so a
// caller cannot, e.g., resurrect a deleted tenant or suspend one mid-teardown.
func TestAllowedStatusTransitions(t *testing.T) {
	ok := []struct{ from, to string }{
		{"active", "suspended"},
		{"suspended", "active"},
		{"active", "deleting"},
		{"suspended", "deleting"},
		{"deleting", "active"},    // cancellation restores prior status
		{"deleting", "suspended"}, // cancellation restores prior status
		{"deleting", "deleted"},   // final drop
	}
	for _, c := range ok {
		if err := validateTransition(c.from, c.to); err != nil {
			t.Errorf("transition %s->%s should be allowed, got %v", c.from, c.to, err)
		}
	}

	bad := []struct{ from, to string }{
		{"active", "deleted"},      // must go through deleting + grace
		{"suspended", "deleted"},   // must go through deleting + grace
		{"deleted", "active"},      // irreversible
		{"deleted", "deleting"},    // irreversible
		{"provisioning", "active"}, // provisioning is the provisioner's job, not this service
		{"active", "provisioning"},
		{"active", "active"}, // no-op is not a transition
		{"deleting", "provisioning"},
	}
	for _, c := range bad {
		if err := validateTransition(c.from, c.to); err == nil {
			t.Errorf("transition %s->%s should be rejected", c.from, c.to)
		}
	}
}

// TestValidateTransitionErrIsTyped lets callers distinguish an illegal transition
// from an infrastructure error.
func TestValidateTransitionErrIsTyped(t *testing.T) {
	err := validateTransition("deleted", "active")
	if !errors.Is(err, errIllegalTransition) {
		t.Fatalf("want errIllegalTransition, got %v", err)
	}
}

// TestGraceDeadline computes the delete-after deadline from a scheduled-at time
// and a grace duration (SPEC-01 §8: default 7 days).
func TestGraceDeadline(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	got := graceDeadline(now, defaultGracePeriod)
	want := now.Add(7 * 24 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("graceDeadline = %v, want %v", got, want)
	}
}

// TestGraceDeadlineZeroUsesDefault proves a zero grace falls back to the 7-day
// default rather than an immediate (already-elapsed) deadline.
func TestGraceDeadlineZeroUsesDefault(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := graceDeadline(now, 0); !got.Equal(now.Add(defaultGracePeriod)) {
		t.Fatalf("zero grace should use default, got %v", got)
	}
}
