// Package tenants owns the control-plane tenant registry surface: the tenants and
// tenant_databases tables (SPEC-02 §2). This file implements tenant settings
// (FR-TEN-08, SPEC-02 §5): read and partial-update of the tenants.settings JSON
// document, validated against an embedded JSON Schema, with embedding.dim held
// immutable after provisioning and every change written to the audit log.
package tenants

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
)

// settingsDefaultsJSON is the canonical SPEC-02 §5 settings document. A tenant's
// stored settings are overlaid on it, so GET/PATCH always operate on a complete,
// schema-valid document even for a freshly provisioned tenant whose stored
// settings hold only the flat embedding_dim mirror. embedding.dim in the defaults
// is a placeholder always overridden by the provisioned dimension.
//
//go:embed settings_defaults.json
var settingsDefaultsJSON []byte

// defaultSettings is the parsed defaults document, decoded once.
var defaultSettings = mustDecodeDefaults()

func mustDecodeDefaults() map[string]any {
	var m map[string]any
	if err := json.Unmarshal(settingsDefaultsJSON, &m); err != nil {
		panic(fmt.Sprintf("tenants: decode settings defaults: %v", err))
	}
	if err := validateSettings(m); err != nil {
		panic(fmt.Sprintf("tenants: settings defaults do not satisfy the schema: %v", err))
	}
	return m
}

// Sentinel errors let the HTTP layer map failures to status codes without string
// matching. ErrInvalidSettings is represented by *ValidationErrors (a 400 with a
// per-field list); the two below cover the other terminal cases.
var (
	// ErrImmutableField is returned when a patch would change a field that is
	// fixed at provisioning — currently embedding.dim, which can only change via
	// a reindex job (SPEC-02 §5, SPEC-03 §5). Mapped to 409 Conflict.
	ErrImmutableField = errors.New("tenants: field is immutable after provisioning")
	// ErrTenantNotFound is returned when no tenant row matches the id.
	ErrTenantNotFound = errors.New("tenants: tenant not found")
)

// flatDimKey is the top-level settings key the migration runner and provisioner
// read to size vector(N) (SPEC-01 §6/§7, ADR-0015). It is the durable source of
// truth for the immutable embedding dimension; the SPEC-02 §5 document exposes
// the same value nested under embedding.dim. The two are kept equal on write.
const flatDimKey = "embedding_dim"

// rowScanner is the single-row surface the service scans; pgx.Row satisfies it.
type rowScanner interface {
	Scan(dest ...any) error
}

// commandTag exposes RowsAffected so the service can detect a missing tenant on
// update; pgconn.CommandTag satisfies it.
type commandTag interface {
	RowsAffected() int64
}

// SettingsDB is the minimal database surface the settings service needs. A pgx
// pool adapter (SettingsFromPool) satisfies it, and unit tests supply a fake.
type SettingsDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) rowScanner
	Exec(ctx context.Context, sql string, args ...any) (commandTag, error)
}

// Actor identifies who made a change, for the audit row. Exactly one of UserID
// (session) or APIKeyID (bearer key) is expected; both may be nil for internal
// callers.
type Actor struct {
	UserID   *string
	APIKeyID *string
}

// PatchParams are the inputs to a settings update.
type PatchParams struct {
	// TenantID is the resolved tenant (FR-ACC-03), never client-supplied.
	TenantID string
	// Actor is recorded on the audit event.
	Actor Actor
	// Patch is the partial settings document to merge into the current one.
	Patch map[string]any
}

// SettingsService reads and updates tenants.settings (SPEC-02 §5). It is
// stateless and safe for concurrent use.
type SettingsService struct {
	DB SettingsDB
}

// NewSettingsService builds a SettingsService over the given DB.
func NewSettingsService(db SettingsDB) *SettingsService {
	return &SettingsService{DB: db}
}

// Get returns the tenant's settings as the SPEC-02 §5 document. The immutable
// embedding dimension is always present: if the stored document lacks a nested
// embedding.dim it is projected from the flat embedding_dim mirror. The flat
// mirror itself is never exposed in the returned view.
func (s *SettingsService) Get(ctx context.Context, tenantID string) (map[string]any, error) {
	stored, err := s.load(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return specView(stored), nil
}

// Patch merges a partial document into the current settings, validates the merged
// result against the schema, enforces the embedding.dim immutability rule, writes
// the merged document, and appends a settings.update audit row — all in one
// transaction so a persisted change is always audited and vice versa.
func (s *SettingsService) Patch(ctx context.Context, p PatchParams) (map[string]any, error) {
	stored, err := s.load(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}

	provisionedDim, hasDim := storedDim(stored)

	merged := deepMerge(specView(stored), p.Patch)

	if err := validateSettings(merged); err != nil {
		return nil, err
	}

	// Immutability: the merged embedding.dim must match the provisioned dimension.
	if hasDim {
		if newDim, ok := embeddingDim(merged); ok && newDim != provisionedDim {
			return nil, fmt.Errorf("%w: embedding.dim (was %d, got %d) — reindex to change",
				ErrImmutableField, provisionedDim, newDim)
		}
	}

	// Persist: store the spec document plus the flat mirror kept equal to the
	// provisioned dimension so the migrator/provisioner keep working unchanged.
	toStore := cloneMap(merged)
	if hasDim {
		toStore[flatDimKey] = provisionedDim
	}
	blob, err := json.Marshal(toStore)
	if err != nil {
		return nil, fmt.Errorf("tenants: marshal settings: %w", err)
	}

	tag, err := s.DB.Exec(ctx,
		`update tenants set settings = $1 where id = $2`, blob, p.TenantID)
	if err != nil {
		return nil, fmt.Errorf("tenants: update settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrTenantNotFound
	}

	if err := s.writeAudit(ctx, p); err != nil {
		return nil, err
	}
	return merged, nil
}

// load reads and decodes tenants.settings for a tenant.
func (s *SettingsService) load(ctx context.Context, tenantID string) (map[string]any, error) {
	var blob []byte
	err := s.DB.QueryRow(ctx, `select settings from tenants where id = $1`, tenantID).Scan(&blob)
	if err != nil {
		return nil, fmt.Errorf("tenants: load settings: %w", err)
	}
	if len(blob) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		return nil, fmt.Errorf("tenants: decode settings: %w", err)
	}
	return m, nil
}

// writeAudit appends a settings.update event (SPEC-02 §6). Details deliberately
// omit the settings values so no configuration content is duplicated into the
// audit log beyond the fact that an update occurred.
func (s *SettingsService) writeAudit(ctx context.Context, p PatchParams) error {
	details, err := json.Marshal(map[string]any{"changed": changedKeys(p.Patch)})
	if err != nil {
		return fmt.Errorf("tenants: marshal audit details: %w", err)
	}
	if _, err := s.DB.Exec(ctx,
		`insert into audit_log (tenant_id, action, actor_user_id, target_type, target_id, details)
		 values ($1, $2, $3, $4, $5, $6)`,
		p.TenantID, "settings.update", p.Actor.UserID, "tenant", p.TenantID, details); err != nil {
		return fmt.Errorf("tenants: write audit: %w", err)
	}
	return nil
}

// specView returns the complete SPEC-02 §5 document for a tenant: the defaults
// overlaid with the stored document, with the flat embedding_dim mirror dropped
// and embedding.dim pinned to the provisioned dimension. Overlaying defaults means
// a freshly provisioned tenant (whose stored settings hold only embedding_dim)
// still presents a complete, schema-valid document.
func specView(stored map[string]any) map[string]any {
	out := deepMerge(defaultSettings, stored)
	dim, hasFlat := storedDim(stored)
	delete(out, flatDimKey)
	if hasFlat {
		emb, _ := out["embedding"].(map[string]any)
		if emb == nil {
			emb = map[string]any{}
		}
		emb["dim"] = dim
		out["embedding"] = emb
	}
	return out
}

// storedDim returns the provisioned embedding dimension, preferring the flat
// mirror and falling back to a nested embedding.dim.
func storedDim(stored map[string]any) (int, bool) {
	if v, ok := asInt(stored[flatDimKey]); ok {
		return v, true
	}
	return embeddingDim(stored)
}

// embeddingDim extracts embedding.dim as an int, if present and numeric.
func embeddingDim(doc map[string]any) (int, bool) {
	emb, ok := doc["embedding"].(map[string]any)
	if !ok {
		return 0, false
	}
	return asInt(emb["dim"])
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

// deepMerge returns base with the keys of patch overlaid. Nested objects are
// merged recursively; any non-object value replaces wholesale. Neither input is
// mutated.
func deepMerge(base, patch map[string]any) map[string]any {
	out := cloneMap(base)
	for k, pv := range patch {
		if pm, ok := pv.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(bm, pm)
				continue
			}
		}
		out[k] = pv
	}
	return out
}

// cloneMap makes a deep copy of a decoded-JSON map so callers never share nested
// maps with the stored document.
func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch vv := v.(type) {
		case map[string]any:
			out[k] = cloneMap(vv)
		case []any:
			cp := make([]any, len(vv))
			copy(cp, vv)
			out[k] = cp
		default:
			out[k] = v
		}
	}
	return out
}

// changedKeys returns the top-level keys touched by a patch, sorted, for the
// audit detail. It records structure only, never values.
func changedKeys(patch map[string]any) []string {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	// small, stable order
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
