package sources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rag-platform/ragctl/internal/crypto"
)

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// Cursor is the opaque keyset position for List pagination: the (created_at, id)
// of the last returned row. It is serialised into the SPEC-07 §1 next_cursor.
type Cursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// Store is the control-plane persistence port for sources and their jobs. PoolDB
// implements it over the control-plane pgx pool; tests use an in-memory fake. All
// methods are tenant-scoped (the tenant id is always an argument, never derived
// from the row) so a caller can never reach another tenant's sources.
type Store interface {
	List(ctx context.Context, tenantID string, limit int, cur *Cursor) ([]Source, error)
	Get(ctx context.Context, tenantID, id string) (Source, error)
	Create(ctx context.Context, p CreateParams) (Source, error)
	Update(ctx context.Context, tenantID, id string, patch UpdatePatch) (Source, error)
	// MarkDeleting flips status to 'deleting'. changed is false when the row was
	// already deleting; existed is false when there is no such source.
	MarkDeleting(ctx context.Context, tenantID, id string) (changed, existed bool, err error)
	// EnqueueJob writes a queued job row. For a sync_source job it returns
	// ErrActiveSyncExists when the partial unique index rejects a second active sync.
	EnqueueJob(ctx context.Context, nj NewJob) (Job, error)
	// FindActiveSync returns an active sync_source job for the source whose payload
	// idempotency_key equals key (empty key => not found), for idempotent replay.
	FindActiveSync(ctx context.Context, tenantID, sourceID, idempotencyKey string) (Job, bool, error)
	// GetCredentials returns the source's sealed credentials_enc ciphertext (nil if
	// none). It is the ONLY read of that column (FR-SRC-10: never returned by the
	// public projection), used by the decrypt-for-Test/Sync path (SPEC-04 §6).
	GetCredentials(ctx context.Context, tenantID, id string) ([]byte, error)
}

// CreateParams is the input to Create. Credentials is the plaintext secret map
// from the request (nil = none); the service seals it and moves the ciphertext
// into CredentialsEnc before calling the Store, so the Store only ever sees
// ciphertext (FR-SRC-10, SPEC-04 §6).
type CreateParams struct {
	TenantID       string
	Kind           string
	Name           string
	Config         json.RawMessage
	ScheduleCron   *string
	Credentials    map[string]string // plaintext in (handler -> service); cleared before the Store call
	CredentialsEnc []byte            // ciphertext out (service -> Store)
}

// UpdatePatch is the set of mutable fields on PATCH. A nil pointer means "leave
// unchanged". ClearSchedule explicitly nulls schedule_cron (manual-only).
type UpdatePatch struct {
	Name           *string
	Config         *json.RawMessage
	Status         *string
	ScheduleCron   *string
	ClearSchedule  bool
	Credentials    map[string]string // plaintext in; sealed by the service (present = replace)
	CredentialsEnc []byte            // ciphertext out (service -> Store); nil = leave unchanged
}

// UpdateParams is the input to Update.
type UpdateParams struct {
	TenantID string
	ID       string
	Patch    UpdatePatch
}

// ListParams selects a tenant's sources with keyset pagination.
type ListParams struct {
	TenantID string
	Limit    int
	Cursor   string
}

// Page is the SPEC-07 §1 pagination envelope: {items, next_cursor}.
type Page struct {
	Items      []Source `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// SyncParams is the input to Sync.
type SyncParams struct {
	TenantID       string
	SourceID       string
	Full           bool
	IdempotencyKey string
}

// NewJob is the input to Store.EnqueueJob.
type NewJob struct {
	TenantID string
	SourceID string
	Kind     string
	Payload  json.RawMessage
}

// Service is the sources domain logic. It holds the Store and an optional
// connector-framework Validator (nil = EPIC-06 seam). It is stateless and safe
// for concurrent use.
type Service struct {
	Store     Store
	Validator Validator
	// Encrypter/Decrypter seal and open source credentials with envelope encryption
	// (SPEC-09 §2, C-4). Injected in internal/cli from the platform Cipher. A nil
	// Encrypter with credentials on the write path fails closed.
	Encrypter Encrypter
	Decrypter Decrypter
	now       func() time.Time
}

// NewService builds a sources service over the given store. Validator is left nil
// (the connector framework lands in EPIC-06); set it to enable kind-specific
// config validation and "test connection".
func NewService(store Store) *Service {
	return &Service{Store: store, now: time.Now}
}

// List returns a tenant's sources newest-first with a next_cursor. It fails
// closed when no tenant is given and rejects a malformed cursor.
func (s *Service) List(ctx context.Context, p ListParams) (Page, error) {
	if p.TenantID == "" {
		return Page{}, fmt.Errorf("sources: tenant is required")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	var cur *Cursor
	if p.Cursor != "" {
		c, err := decodeCursor(p.Cursor)
		if err != nil {
			return Page{}, invalid("invalid cursor")
		}
		cur = c
	}

	// Fetch one extra row to know whether a further page exists.
	rows, err := s.Store.List(ctx, p.TenantID, limit+1, cur)
	if err != nil {
		return Page{}, fmt.Errorf("sources: list: %w", err)
	}
	page := Page{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	if page.Items == nil {
		page.Items = []Source{}
	}
	return page, nil
}

// Get returns one source scoped to the tenant, or ErrNotFound.
func (s *Service) Get(ctx context.Context, tenantID, id string) (Source, error) {
	if tenantID == "" {
		return Source{}, fmt.Errorf("sources: tenant is required")
	}
	return s.Store.Get(ctx, tenantID, id)
}

// Create validates and persists a new source. Generic validation (kind, name,
// config shape) always applies; when a connector Validator is wired it also runs
// the kind-specific ValidateConfig (FR-SRC-14). New sources start `active`.
func (s *Service) Create(ctx context.Context, p CreateParams) (Source, error) {
	if p.TenantID == "" {
		return Source{}, fmt.Errorf("sources: tenant is required")
	}
	if err := validateKind(p.Kind); err != nil {
		return Source{}, err
	}
	if err := validateName(p.Name); err != nil {
		return Source{}, err
	}
	if err := validateConfigShape(p.Config); err != nil {
		return Source{}, err
	}
	if s.Validator != nil {
		if err := s.Validator.ValidateConfig(p.Kind, p.Config); err != nil {
			return Source{}, invalid("connector config invalid: %v", err)
		}
	}
	if len(p.Credentials) > 0 {
		enc, err := s.sealCredentials(p.Credentials)
		if err != nil {
			return Source{}, err
		}
		p.CredentialsEnc = enc
	}
	p.Credentials = nil // drop the plaintext reference before the Store ever sees the params
	return s.Store.Create(ctx, p)
}

// Update applies a patch to an existing source. Only active/paused are settable
// via the API (pause/resume, FR-SRC-01); status=deleting/error are system-managed.
// When a Validator is wired and config changes, the new config is re-validated.
func (s *Service) Update(ctx context.Context, p UpdateParams) (Source, error) {
	if p.TenantID == "" {
		return Source{}, fmt.Errorf("sources: tenant is required")
	}
	if p.Patch.Name != nil {
		if err := validateName(*p.Patch.Name); err != nil {
			return Source{}, err
		}
	}
	if p.Patch.Status != nil && !apiSettableStatuses[*p.Patch.Status] {
		return Source{}, invalid("status must be one of active, paused")
	}
	if p.Patch.Config != nil {
		if err := validateConfigShape(*p.Patch.Config); err != nil {
			return Source{}, err
		}
	}
	if s.Validator != nil && p.Patch.Config != nil {
		// Re-validate against the existing kind.
		existing, err := s.Store.Get(ctx, p.TenantID, p.ID)
		if err != nil {
			return Source{}, err
		}
		if err := s.Validator.ValidateConfig(existing.Kind, *p.Patch.Config); err != nil {
			return Source{}, invalid("connector config invalid: %v", err)
		}
	}
	if len(p.Patch.Credentials) > 0 {
		enc, err := s.sealCredentials(p.Patch.Credentials)
		if err != nil {
			return Source{}, err
		}
		p.Patch.CredentialsEnc = enc
	}
	p.Patch.Credentials = nil // drop the plaintext reference before the Store ever sees the patch
	return s.Store.Update(ctx, p.TenantID, p.ID, p.Patch)
}

// Delete soft-deletes a source: it marks the row `deleting` and enqueues a
// `delete_source` job whose worker removes the source's documents and chunks
// (FR-SRC-12, EPIC-09 STORY-09.6). It is idempotent — a source already deleting
// returns its status change without enqueuing a second job — and returns
// ErrNotFound for an unknown source.
func (s *Service) Delete(ctx context.Context, tenantID, id string) (Job, error) {
	if tenantID == "" {
		return Job{}, fmt.Errorf("sources: tenant is required")
	}
	changed, existed, err := s.Store.MarkDeleting(ctx, tenantID, id)
	if err != nil {
		return Job{}, fmt.Errorf("sources: mark deleting: %w", err)
	}
	if !existed {
		return Job{}, ErrNotFound
	}
	if !changed {
		// Already deleting: idempotent no-op, no duplicate job.
		return Job{}, nil
	}
	payload, _ := json.Marshal(map[string]any{"source_id": id})
	return s.Store.EnqueueJob(ctx, NewJob{TenantID: tenantID, SourceID: id, Kind: "delete_source", Payload: payload})
}

// Sync enqueues a manual `sync_source` job for the source (FR-SRC-11). It returns
// ErrNotFound for an unknown source and ErrActiveSyncExists (SPEC-07 §2 409) when
// a sync is already queued or running. An Idempotency-Key replays the existing
// active job rather than conflicting, so a client retry is safe (SPEC-07 §1).
func (s *Service) Sync(ctx context.Context, p SyncParams) (Job, error) {
	if p.TenantID == "" {
		return Job{}, fmt.Errorf("sources: tenant is required")
	}
	if _, err := s.Store.Get(ctx, p.TenantID, p.SourceID); err != nil {
		return Job{}, err // ErrNotFound
	}
	if p.IdempotencyKey != "" {
		if existing, ok, err := s.Store.FindActiveSync(ctx, p.TenantID, p.SourceID, p.IdempotencyKey); err != nil {
			return Job{}, fmt.Errorf("sources: idempotency lookup: %w", err)
		} else if ok {
			return existing, nil // idempotent replay
		}
	}
	payload := map[string]any{"full": p.Full}
	if p.IdempotencyKey != "" {
		payload["idempotency_key"] = p.IdempotencyKey
	}
	body, _ := json.Marshal(payload)
	job, err := s.Store.EnqueueJob(ctx, NewJob{TenantID: p.TenantID, SourceID: p.SourceID, Kind: "sync_source", Payload: body})
	if err != nil {
		if errors.Is(err, ErrActiveSyncExists) {
			return Job{}, ErrActiveSyncExists
		}
		return Job{}, fmt.Errorf("sources: enqueue sync: %w", err)
	}
	return job, nil
}

// Test runs the connector "test connection" for a source (FR-SRC-14). It returns
// ErrConnectorUnavailable when the connector framework is not wired (EPIC-06
// seam) and ErrNotFound for an unknown source.
//
// A connector probe FAILURE — an unreachable host or rejected credentials — is a
// client-facing, actionable error about the admin's own source config, not an
// internal fault: it is wrapped as a *ValidationError so the handler surfaces the
// connector's message (400) instead of genericising it to 500 (ISSUE-0026,
// ADR-0050 §"HTTP surfacing"). The connector already sanitises its Test errors at
// the connector boundary (ADR-0050: never echoes the raw error or a URL query, C-4),
// so the message is safe to return verbatim. The seam sentinels (ErrConnectorUnavailable
// for an unregistered kind, ErrNotFound) are preserved unwrapped so they keep their
// own statuses. A genuine internal fault (credential decrypt, store failure) occurs
// before the probe and propagates unchanged to the 500 fallback.
func (s *Service) Test(ctx context.Context, tenantID, id string) error {
	if tenantID == "" {
		return fmt.Errorf("sources: tenant is required")
	}
	if s.Validator == nil {
		return ErrConnectorUnavailable
	}
	src, err := s.Store.Get(ctx, tenantID, id)
	if err != nil {
		return err // ErrNotFound
	}
	enc, err := s.Store.GetCredentials(ctx, tenantID, id)
	if err != nil {
		return err
	}
	creds, zero, err := s.openCredentials(enc)
	if err != nil {
		return err
	}
	defer zero() // clear the decrypted secret the moment Test returns (SPEC-04 §6)
	if err := s.Validator.Test(ctx, src.Kind, src.Config, creds); err != nil {
		// Preserve the seam sentinels; wrap a real probe failure as an actionable
		// client-facing validation error (mirrors the Create path's ValidateConfig wrap).
		if errors.Is(err, ErrConnectorUnavailable) || errors.Is(err, ErrNotFound) {
			return err
		}
		return invalid("%s", err.Error())
	}
	return nil
}

// sealCredentials marshals a plaintext credential map and envelope-encrypts it
// (SPEC-09 §2, C-4). The intermediate plaintext JSON buffer is zeroed as soon as
// it is sealed. It fails closed when no Encrypter is wired.
func (s *Service) sealCredentials(creds map[string]string) ([]byte, error) {
	if s.Encrypter == nil {
		return nil, fmt.Errorf("sources: credential encryption is not configured")
	}
	pt, err := json.Marshal(creds)
	if err != nil {
		// Never include the value in the error (sanitised, SPEC-04 §6).
		return nil, fmt.Errorf("sources: marshal credentials")
	}
	enc, err := s.Encrypter.Encrypt(pt)
	crypto.Zero(pt)
	if err != nil {
		return nil, fmt.Errorf("sources: seal credentials: %w", err)
	}
	return enc, nil
}

// openCredentials decrypts a source's sealed credentials into a map for the
// duration of a Test/Sync (SPEC-04 §6). The returned zero func clears the map;
// the caller MUST defer it. The decrypted JSON buffer is zeroed here immediately
// after unmarshalling. Errors are sanitised: they never carry the ciphertext or a
// secret value. An empty ciphertext yields a nil map and a no-op zero.
//
// ponytail: Go strings (the map values) cannot be overwritten in place, so
// "zeroed after use" here means the decrypted []byte buffer is wiped and the map
// is cleared (values become unreferenced and GC-eligible). Wiping the string
// bytes themselves would need a []byte-valued credential type across the connector
// interface — the upgrade path if a stronger guarantee is ever required.
func (s *Service) openCredentials(enc []byte) (map[string]string, func(), error) {
	noop := func() {}
	if len(enc) == 0 {
		return nil, noop, nil
	}
	if s.Decrypter == nil {
		return nil, noop, fmt.Errorf("sources: credential decryption is not configured")
	}
	pt, err := s.Decrypter.Decrypt(enc)
	if err != nil {
		return nil, noop, fmt.Errorf("sources: open credentials")
	}
	creds := map[string]string{}
	if err := json.Unmarshal(pt, &creds); err != nil {
		crypto.Zero(pt)
		return nil, noop, fmt.Errorf("sources: stored credentials malformed")
	}
	crypto.Zero(pt)
	return creds, func() { clear(creds) }, nil
}

// encodeCursor serialises a Cursor to an opaque base64url token.
func encodeCursor(c Cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a base64url cursor token.
func decodeCursor(s string) (*Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
