package auth

import "time"

// LockoutPolicy is the account-lockout rule (SPEC-09 §3): after MaxFailures
// failed logins the account is locked for Window.
type LockoutPolicy struct {
	MaxFailures int
	Window      time.Duration
}

// DefaultLockoutPolicy returns the SPEC-09 §3 policy: 10 failures / 15 minutes.
func DefaultLockoutPolicy() LockoutPolicy {
	return LockoutPolicy{MaxFailures: 10, Window: 15 * time.Minute}
}

// NextFailure decides, given the new failure count (the stored count already
// incremented for this attempt), whether the account is now locked and, if so,
// until when. The count reaching MaxFailures triggers a lock for Window from now.
func (p LockoutPolicy) NextFailure(failureCount int, now time.Time) (locked bool, until time.Time) {
	if failureCount >= p.MaxFailures {
		return true, now.Add(p.Window)
	}
	return false, time.Time{}
}

// IsLocked reports whether a stored lock deadline is still in the future
// relative to now. A zero deadline means never locked; an expired deadline means
// the lock has lapsed.
func IsLocked(lockedUntil, now time.Time) bool {
	return !lockedUntil.IsZero() && lockedUntil.After(now)
}
