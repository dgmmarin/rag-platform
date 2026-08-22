package auth

import (
	"testing"
	"time"
)

func TestLockoutPolicyDefaults(t *testing.T) {
	p := DefaultLockoutPolicy()
	if p.MaxFailures != 10 {
		t.Fatalf("MaxFailures = %d, want 10 (SPEC-09 §3)", p.MaxFailures)
	}
	if p.Window != 15*time.Minute {
		t.Fatalf("Window = %v, want 15m (SPEC-09 §3)", p.Window)
	}
}

// TestNextFailureLocksAfterThreshold proves the Nth failure within the window
// sets a lockout deadline; earlier failures do not.
func TestNextFailureLocksAfterThreshold(t *testing.T) {
	p := DefaultLockoutPolicy()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// 9th failure: below threshold, no lock.
	locked, until := p.NextFailure(9, now)
	if locked {
		t.Fatalf("locked at 9 failures (count becomes 10 exactly at threshold, should still allow that attempt): until=%v", until)
	}

	// 10th failure reaches the threshold and locks.
	locked, until = p.NextFailure(10, now)
	if !locked {
		t.Fatal("not locked at the 10th failure")
	}
	if want := now.Add(15 * time.Minute); !until.Equal(want) {
		t.Fatalf("lock until = %v, want %v", until, want)
	}
}

// TestIsLocked reports the current lock state given a stored deadline.
func TestIsLocked(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	if IsLocked(time.Time{}, now) {
		t.Fatal("zero deadline should not be locked")
	}
	if !IsLocked(now.Add(time.Minute), now) {
		t.Fatal("future deadline should be locked")
	}
	if IsLocked(now.Add(-time.Minute), now) {
		t.Fatal("past deadline should not be locked (expired)")
	}
}
