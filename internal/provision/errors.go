package provision

// Exported error sentinels so callers outside this package — notably the
// STORY-04.6 admin tenant HTTP handlers (internal/cp/tenants) — can classify a
// provisioning/lifecycle failure and map it to the right SPEC-07 §1 status code
// with errors.Is, without string matching. These are aliases of the existing
// unexported sentinels, so every error the Provisioner/Lifecycle already wraps
// keeps matching; nothing about the existing behaviour changes.
var (
	// ErrValidation tags a rejected input (bad/missing parameters, an empty move,
	// a negative grace). HTTP callers map it to 400 validation.
	ErrValidation = errValidation
	// ErrIllegalTransition tags a status change the lifecycle state machine
	// forbids (e.g. suspending an already-suspended tenant, deleting a deleted
	// one). HTTP callers map it to 409 conflict.
	ErrIllegalTransition = errIllegalTransition
)
