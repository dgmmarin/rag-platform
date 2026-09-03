package tenants

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rag-platform/ragctl/internal/provision"
)

// This file implements the platform-admin tenant surface (STORY-04.6,
// FR-TEN-01/05/07, SPEC-07 §2). These endpoints live under /admin, require
// is_platform_admin, and are NOT tenant-scoped: unlike the /v1 surface, the
// tenant is a route/body value here because a platform admin operates ACROSS
// tenants (there is no per-request tenant to derive — FR-ACC-03 governs the
// tenant-scoped /v1 routes, not the platform surface).
//
// The lifecycle already exists (STORY-02.3/02.4/02.5, ADR-0016/0017): this is a
// thin orchestration over provision.Provisioner (enrol) and provision.Lifecycle
// (suspend/resume/move/schedule-delete) plus the settings service (FR-TEN-08).
// It opens no tenant database — provisioning/lifecycle use their own privileged
// connection and the registry reads use the control-plane pool (C-3).

const (
	defaultAdminListLimit = 50
	maxAdminListLimit     = 200
)

// Tenant is the control-plane registry view returned by the admin tenant API. It
// carries NO connection details or secrets (C-4/SPEC-09 §2): host/port/password
// live in tenant_databases and are never exposed here.
type Tenant struct {
	ID          string     `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Region      string     `json:"region"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
	DeleteAfter *time.Time `json:"delete_after,omitempty"`
}

// tenantCursor is the opaque keyset position for List pagination: the
// (created_at, id) of the last returned row.
type tenantCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// TenantPage is the SPEC-07 §1 pagination envelope: {items, next_cursor}.
type TenantPage struct {
	Items      []Tenant `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// adminValidationError is a client-facing input error carrying a safe message;
// the handlers surface it as 400 validation.
type adminValidationError struct{ msg string }

func (e *adminValidationError) Error() string { return "tenants: " + e.msg }

func invalidTenant(format string, a ...any) *adminValidationError {
	return &adminValidationError{msg: fmt.Sprintf(format, a...)}
}

// Provisioner is the enrol side (POST /admin/tenants). *provision.Provisioner
// satisfies it; tests use a fake so the service is unit-testable without a real
// Postgres cluster.
type Provisioner interface {
	Provision(ctx context.Context, p provision.Params) (provision.Result, error)
}

// Lifecycle is the status/connection side (PATCH/DELETE). *provision.Lifecycle
// satisfies it.
type Lifecycle interface {
	Suspend(ctx context.Context, slug string) (provision.LifecycleResult, error)
	Resume(ctx context.Context, slug string) (provision.LifecycleResult, error)
	ScheduleDelete(ctx context.Context, slug string, grace time.Duration) (provision.LifecycleResult, error)
	Move(ctx context.Context, slug string, params provision.MoveParams) (provision.LifecycleResult, error)
}

// AdminStore is the control-plane registry read/record surface the admin tenant
// service needs beyond what the Provisioner/Lifecycle already own. AdminPoolStore
// implements it over the control-plane pool; tests use a fake.
type AdminStore interface {
	List(ctx context.Context, limit int, cur *tenantCursor) ([]Tenant, error)
	GetByID(ctx context.Context, id string) (Tenant, error)
	// RecordProvisionJob writes the provision_tenant mirror row for a
	// just-provisioned tenant and returns its id (the job id the create response
	// carries; SPEC-07 §2 "returns tenant + job id").
	RecordProvisionJob(ctx context.Context, tenantID string) (string, error)
}

// AdminService is the platform-admin tenant domain logic. It is stateless and
// safe for concurrent use.
type AdminService struct {
	Store    AdminStore
	Prov     Provisioner
	Life     Lifecycle
	Settings *SettingsService // used for the PATCH settings sub-change (FR-TEN-08)
	// SSLMode is the sslmode recorded for a newly provisioned tenant's database
	// connection (SPEC-01 §4), mirroring `ragctl enroll --db-ssl-mode`. Empty lets
	// the provisioner apply its default ("require"). Set to "disable" for a local,
	// non-TLS cluster.
	SSLMode string
}

// CreateTenantParams is the input to Create (POST /admin/tenants).
type CreateTenantParams struct {
	Slug         string
	Name         string
	Region       string
	EmbeddingDim int
}

// CreateResult is the POST /admin/tenants response body: the provisioned tenant
// plus the provision job id.
type CreateResult struct {
	Tenant Tenant
	JobID  string
}

// Create enrols a new tenant (FR-TEN-01/02). The async River `provision_tenant`
// job EXECUTION is EPIC-09 (ADR-0005); until then, exactly as `ragctl enroll`
// established (STORY-02.3, ADR-0016), it runs the same idempotent Provisioner
// synchronously and records a provision_tenant mirror row so the response carries
// a job id (the jobs table is the history/mirror view — ADR-0005). The tenant is
// provisioned and active before the response returns, so the mirror row is a
// truthful `succeeded` record, not a perpetually-queued placeholder. When EPIC-09
// wires River, this becomes a real async enqueue consumed by the worker.
func (s *AdminService) Create(ctx context.Context, p CreateTenantParams) (CreateResult, error) {
	if strings.TrimSpace(p.Slug) == "" {
		return CreateResult{}, invalidTenant("slug is required")
	}
	if strings.TrimSpace(p.Name) == "" {
		return CreateResult{}, invalidTenant("name is required")
	}
	if p.EmbeddingDim < 0 {
		return CreateResult{}, invalidTenant("embedding_dim must not be negative")
	}
	res, err := s.Prov.Provision(ctx, provision.Params{
		Slug:         p.Slug,
		Name:         p.Name,
		Region:       p.Region,
		EmbeddingDim: p.EmbeddingDim,
		SSLMode:      s.SSLMode,
	})
	if err != nil {
		return CreateResult{}, err
	}
	jobID, err := s.Store.RecordProvisionJob(ctx, res.TenantID)
	if err != nil {
		return CreateResult{}, fmt.Errorf("tenants: record provision job: %w", err)
	}
	t, err := s.Store.GetByID(ctx, res.TenantID)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Tenant: t, JobID: jobID}, nil
}

// ListTenantsParams selects a page of the registry with keyset pagination.
type ListTenantsParams struct {
	Limit  int
	Cursor string
}

// List returns tenants newest-first (by created_at) with a next_cursor.
func (s *AdminService) List(ctx context.Context, p ListTenantsParams) (TenantPage, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = defaultAdminListLimit
	}
	if limit > maxAdminListLimit {
		limit = maxAdminListLimit
	}

	var cur *tenantCursor
	if p.Cursor != "" {
		c, err := decodeTenantCursor(p.Cursor)
		if err != nil {
			return TenantPage{}, invalidTenant("invalid cursor")
		}
		cur = c
	}

	// Fetch one extra row to know whether a further page exists.
	rows, err := s.Store.List(ctx, limit+1, cur)
	if err != nil {
		return TenantPage{}, fmt.Errorf("tenants: list: %w", err)
	}
	page := TenantPage{Items: rows}
	if len(rows) > limit {
		page.Items = rows[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeTenantCursor(tenantCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	if page.Items == nil {
		page.Items = []Tenant{}
	}
	return page, nil
}

// ConnectionPatch is the optional db-connection sub-change of PATCH. A nil field
// leaves the stored value unchanged (mirrors provision.MoveParams). Password, if
// set, is envelope-encrypted by Move and never logged or returned (C-4).
type ConnectionPatch struct {
	Host     *string `json:"host"`
	Port     *int    `json:"port"`
	Database *string `json:"database"`
	Username *string `json:"username"`
	SSLMode  *string `json:"ssl_mode"`
	Password *string `json:"password"`
}

func (c *ConnectionPatch) toMoveParams() provision.MoveParams {
	var mp provision.MoveParams
	if c.Host != nil {
		mp.Host = *c.Host
	}
	if c.Port != nil {
		mp.Port = *c.Port
	}
	if c.Database != nil {
		mp.Database = *c.Database
	}
	if c.Username != nil {
		mp.Username = *c.Username
	}
	if c.SSLMode != nil {
		mp.SSLMode = *c.SSLMode
	}
	if c.Password != nil {
		mp.Password = *c.Password
	}
	return mp
}

// PatchTenantParams is the input to Patch (PATCH /admin/tenants/{id}). Any subset
// of the three sub-changes may be present; at least one must be.
type PatchTenantParams struct {
	ID         string
	Status     *string          // "active" (resume) or "suspended" (suspend)
	Connection *ConnectionPatch // db-connection move (FR-TEN-07)
	Settings   map[string]any   // partial settings document (FR-TEN-08)
	Actor      Actor            // recorded on the settings audit event
}

// Patch applies the requested sub-changes to a tenant (FR-TEN-04/07/08). It
// resolves the tenant by id first (so an unknown id is ErrTenantNotFound before
// any write), then routes each present sub-change to the existing service that
// owns it: settings -> SettingsService.Patch, connection -> Lifecycle.Move,
// status -> Lifecycle.Suspend/Resume. Each already writes its own audit event
// (settings.update / tenant.move / tenant.suspend|resume), so Patch adds none.
func (s *AdminService) Patch(ctx context.Context, p PatchTenantParams) (Tenant, error) {
	if p.Status == nil && p.Connection == nil && len(p.Settings) == 0 {
		return Tenant{}, invalidTenant("patch must set at least one of status, connection, settings")
	}
	if p.Status != nil && *p.Status != "active" && *p.Status != "suspended" {
		return Tenant{}, invalidTenant("status must be one of active, suspended")
	}

	cur, err := s.Store.GetByID(ctx, p.ID) // ErrTenantNotFound for an unknown id
	if err != nil {
		return Tenant{}, err
	}

	if len(p.Settings) > 0 {
		if s.Settings == nil {
			return Tenant{}, fmt.Errorf("tenants: settings service not configured")
		}
		if _, err := s.Settings.Patch(ctx, PatchParams{TenantID: p.ID, Actor: p.Actor, Patch: p.Settings}); err != nil {
			return Tenant{}, err
		}
	}
	if p.Connection != nil {
		if _, err := s.Life.Move(ctx, cur.Slug, p.Connection.toMoveParams()); err != nil {
			return Tenant{}, err
		}
	}
	if p.Status != nil {
		switch *p.Status {
		case "suspended":
			_, err = s.Life.Suspend(ctx, cur.Slug)
		case "active":
			_, err = s.Life.Resume(ctx, cur.Slug)
		}
		if err != nil {
			return Tenant{}, err
		}
	}
	return s.Store.GetByID(ctx, p.ID)
}

// Delete schedules a tenant's deletion with a grace period (FR-TEN-05). A zero
// grace uses the lifecycle default (7 days). The irreversible teardown runs after
// the grace window via the EPIC-09 River `delete_tenant` job (ADR-0005); today
// ScheduleDelete is the complete, real action (status -> deleting, delete_after
// recorded, resolver evicts the tenant within ~1s). It resolves the tenant by id
// first so an unknown id is ErrTenantNotFound. ScheduleDelete writes its own
// tenant.delete.schedule audit event, so Delete adds none.
func (s *AdminService) Delete(ctx context.Context, id string, grace time.Duration) (Tenant, error) {
	cur, err := s.Store.GetByID(ctx, id)
	if err != nil {
		return Tenant{}, err
	}
	if _, err := s.Life.ScheduleDelete(ctx, cur.Slug, grace); err != nil {
		return Tenant{}, err
	}
	return s.Store.GetByID(ctx, id)
}

// encodeTenantCursor serialises a cursor to an opaque base64url token.
func encodeTenantCursor(c tenantCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeTenantCursor parses a base64url cursor token.
func decodeTenantCursor(s string) (*tenantCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var c tenantCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
