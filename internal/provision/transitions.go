package provision

import (
	"errors"
	"fmt"
	"time"
)

// errIllegalTransition tags a rejected tenant status transition so callers (and
// tests) can distinguish an illegal request from an infrastructure failure.
var errIllegalTransition = errors.New("provision: illegal tenant status transition")

// defaultGracePeriod is the SPEC-01 §8 / FR-TEN-05 grace window (7 days) between
// scheduling a deletion and the irreversible drop, during which a platform admin
// can cancel.
const defaultGracePeriod = 7 * 24 * time.Hour

// allowedTransitions is the tenant lifecycle state machine this service owns
// (SPEC-01 §8). Provisioning (-> active) is the provisioner's job, not this
// service's, so it is deliberately absent. Entries map a source status to the
// set of statuses it may move to.
var allowedTransitions = map[string]map[string]bool{
	"active":    {"suspended": true, "deleting": true},
	"suspended": {"active": true, "deleting": true},
	// deleting can be cancelled back to a serving status, or completed to deleted.
	"deleting": {"active": true, "suspended": true, "deleted": true},
	// deleted is terminal and irreversible (FR-TEN-05).
}

// validateTransition reports whether moving a tenant from -> to is permitted by
// the lifecycle state machine. A no-op (from == to) is not a transition and is
// rejected so callers do not silently issue empty updates.
func validateTransition(from, to string) error {
	if from == to {
		return fmt.Errorf("%w: %s is already %s", errIllegalTransition, from, to)
	}
	if allowedTransitions[from][to] {
		return nil
	}
	return fmt.Errorf("%w: %s -> %s", errIllegalTransition, from, to)
}

// graceDeadline returns the instant after which a scheduled deletion may proceed:
// now + grace. A non-positive grace falls back to the default so a
// mis-configured caller cannot accidentally request an immediate, unrecoverable
// drop (fail safe).
func graceDeadline(now time.Time, grace time.Duration) time.Time {
	if grace <= 0 {
		grace = defaultGracePeriod
	}
	return now.Add(grace)
}
