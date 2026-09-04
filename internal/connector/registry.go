package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Registry maps a Kind to a factory that produces a fresh Connector. A factory is
// used (not a shared instance) because a connector may hold per-sync state
// (SPEC-04 §1). Registry is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	factories map[Kind]func() Connector
}

// NewRegistry returns an empty registry. Tests use an isolated one; production
// uses the package-level default registry (DefaultRegistry).
func NewRegistry() *Registry {
	return &Registry{factories: make(map[Kind]func() Connector)}
}

// Register adds a connector factory for a kind. It panics on a nil factory or a
// duplicate kind: registration happens at package init (SPEC-04 §1), so a
// misconfiguration must fail loudly at startup, not silently at request time.
func (r *Registry) Register(kind Kind, factory func() Connector) {
	if factory == nil {
		panic(fmt.Sprintf("connector: nil factory for kind %q", kind))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[kind]; exists {
		panic(fmt.Sprintf("connector: kind %q already registered", kind))
	}
	r.factories[kind] = factory
}

// Lookup returns a fresh Connector for the kind, or ok=false if none is registered.
func (r *Registry) Lookup(kind Kind) (Connector, bool) {
	r.mu.RLock()
	factory, ok := r.factories[kind]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return factory(), true
}

// Kinds returns the registered kinds, sorted, for diagnostics and enumeration.
func (r *Registry) Kinds() []Kind {
	r.mu.RLock()
	kinds := make([]Kind, 0, len(r.factories))
	for k := range r.factories {
		kinds = append(kinds, k)
	}
	r.mu.RUnlock()
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}

// defaultRegistry is the process-wide registry connectors register into from their
// package init (SPEC-04 §1: `connector.Register(...)`), and that the worker
// resolves against by sources.kind.
var defaultRegistry = NewRegistry()

// DefaultRegistry returns the process-wide registry.
func DefaultRegistry() *Registry { return defaultRegistry }

// Register adds a connector factory to the default registry (SPEC-04 §1).
func Register(kind Kind, factory func() Connector) { defaultRegistry.Register(kind, factory) }

// Lookup resolves a kind against the default registry.
func Lookup(kind Kind) (Connector, bool) { return defaultRegistry.Lookup(kind) }

// SourcesValidator adapts a Registry to the control-plane sources "Validator" seam
// (internal/cp/sources, STORY-04.3): it satisfies that package's local interface
// structurally, so wiring it enables kind-specific config validation and "test
// connection" on the sources API with no change to the sources package
// (NFR-MNT-01). The sources package stays free of any connector import; the
// dependency is injected at the composition root (internal/cli).
type SourcesValidator struct {
	reg *Registry
	// unavailable is returned by Test when no connector is registered for the kind.
	// The caller (internal/cli) supplies sources.ErrConnectorUnavailable so the
	// sources handler maps it to the not_found "seam" envelope — the same behaviour
	// as before any connector for that kind exists.
	unavailable error
}

// NewSourcesValidator builds the sources-seam adapter over a registry. unavailable
// is the error Test returns for a kind with no registered connector.
func NewSourcesValidator(reg *Registry, unavailable error) SourcesValidator {
	return SourcesValidator{reg: reg, unavailable: unavailable}
}

// ValidateConfig runs the kind's connector ValidateConfig. When no connector is
// registered for the kind (a valid enum kind whose connector is not built yet), it
// returns nil — kind-specific validation is deferred and the sources package's
// generic validation still applies — so a source whose connector does not exist
// yet can still be created.
func (v SourcesValidator) ValidateConfig(kind string, cfg json.RawMessage) error {
	c, ok := v.reg.Lookup(Kind(kind))
	if !ok {
		return nil
	}
	return c.ValidateConfig(cfg)
}

// Test runs the kind's connector "test connection" (FR-SRC-14) with the source's
// decrypted credentials (STORY-06.2, SPEC-04 §6). The sources package decrypts
// credentials_enc and passes the plaintext map here; this adapter converts it to
// the connector Credentials type and forwards it. When no connector is registered
// for the kind it returns the injected unavailable sentinel and touches no
// credentials. The sources package owns the credential lifecycle (it zeroes them
// after Test returns), so this adapter neither logs nor retains them.
func (v SourcesValidator) Test(ctx context.Context, kind string, cfg json.RawMessage, creds map[string]string) error {
	c, ok := v.reg.Lookup(Kind(kind))
	if !ok {
		if v.unavailable != nil {
			return v.unavailable
		}
		return ErrUnsupportedKind
	}
	return c.Test(ctx, cfg, Credentials(creds))
}

// Ensure the adapter keeps satisfying the shape the sources seam expects even as
// this package evolves (compile-time guard against accidental signature drift).
var _ interface {
	ValidateConfig(kind string, cfg json.RawMessage) error
	Test(ctx context.Context, kind string, cfg json.RawMessage, creds map[string]string) error
} = SourcesValidator{}
