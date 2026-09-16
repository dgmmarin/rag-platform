package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// APIKeyHandlers are the HTTP entry points for the session-admin api-keys surface
// (STORY-11.5, ISSUE-0064, SPEC-02 §2, FR-ACC-04). They wrap the existing
// APIKeyService with no minting/storage logic of their own. The plaintext secret
// is returned exactly ONCE, by Create; List never carries it (C-4, never logged).
// The tenant is taken from the resolved context (FR-ACC-03).
type APIKeyHandlers struct {
	Service *APIKeyService
}

// NewAPIKeyHandlers builds handlers over an api-key service.
func NewAPIKeyHandlers(svc *APIKeyService) *APIKeyHandlers {
	return &APIKeyHandlers{Service: svc}
}

// apiKeyView is a stored key as returned to the admin UI. It has no secret field:
// the plaintext exists only in Create's `key` field, once (FR-ACC-04).
type apiKeyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// toAPIKeyView adds the JSON tags apiKeyView carries; the fields are otherwise
// identical to APIKeyRecord, so a direct conversion suffices (and neither type
// has a secret field — the plaintext is only ever in Create's `key`).
func toAPIKeyView(r APIKeyRecord) apiKeyView {
	return apiKeyView(r)
}

// List serves GET .../api-keys: the tenant's keys, never a secret.
func (h *APIKeyHandlers) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	recs, err := h.Service.List(r.Context(), tid.String())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list api keys")
		return
	}
	out := make([]apiKeyView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, toAPIKeyView(rec))
	}
	writeJSONStatus(w, http.StatusOK, out)
}

// createKeyRequest is the POST .../api-keys body. `expires_at` is optional
// RFC3339; `scopes` is validated against the SPEC-07 §2 set.
type createKeyRequest struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expires_at,omitempty"`
}

// createKeyResponse returns the plaintext secret ONCE alongside the stored record
// (FR-ACC-04). The secret is never logged and never re-derivable after this call.
type createKeyResponse struct {
	Key    string     `json:"key"`
	Record apiKeyView `json:"record"`
}

// Create serves POST .../api-keys: mint a key and return its plaintext secret
// once. Bad scope/name/expiry are 400 before any write.
func (h *APIKeyHandlers) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if _, err := ParseScopes(req.Scopes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid scopes")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "expires_at must be RFC3339")
			return
		}
		expiresAt = &t
	}

	params := CreateKeyParams{
		TenantID:  tid.String(),
		Name:      req.Name,
		Scopes:    req.Scopes,
		ExpiresAt: expiresAt,
	}
	if sess, ok := SessionFrom(r.Context()); ok && sess.UserID != "" {
		uid := sess.UserID
		params.CreatedBy = &uid
	}

	rec, secret, err := h.Service.Create(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create api key")
		return
	}
	writeJSONStatus(w, http.StatusCreated, createKeyResponse{Key: secret, Record: toAPIKeyView(rec)})
}

// Revoke serves DELETE .../api-keys/{keyId}: revoke a key. An unknown key is 404;
// re-revoking is an idempotent 204.
func (h *APIKeyHandlers) Revoke(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenant.TenantIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no tenant resolved")
		return
	}
	keyID := r.PathValue("keyId")
	if err := h.Service.Revoke(r.Context(), tid.String(), keyID); err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			writeError(w, http.StatusNotFound, "api key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not revoke api key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
