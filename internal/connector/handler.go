package connector

import (
	"encoding/json"
	"net/http"
)

// Handlers is the HTTP entry point for the connector-kinds schema endpoint
// (SPEC-11 §10, ADR-0075, STORY-11.2). GET /admin/connector-kinds is
// session-authenticated and platform-global (RequireSession only — no tenant, no
// CSRF on a GET, SPEC-07 §1): the admin UI reads it once to render every kind's
// create/edit source form (SPEC-11 §10.1), so it carries no tenant-scoped state.
type Handlers struct {
	Registry *Registry
}

// NewHandlers builds handlers over a connector registry (DefaultRegistry() in
// production).
func NewHandlers(reg *Registry) *Handlers { return &Handlers{Registry: reg} }

// kindsResponse is the GET /admin/connector-kinds body (SPEC-11 §10):
// { "kinds": [ { "kind", "label", "fields": [ {"name","label","type","required"} ] } ] }
type kindsResponse struct {
	Kinds []kindDTO `json:"kinds"`
}

type kindDTO struct {
	Kind   string     `json:"kind"`
	Label  string     `json:"label"`
	Fields []fieldDTO `json:"fields"`
}

type fieldDTO struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// List serves GET /admin/connector-kinds: every registered connector kind's
// form-field DESCRIPTORS. A secret field (Type:"secret") never carries a value —
// only the fact that the admin UI must render it write-only (SPEC-04 §6) — so this
// handler never has a credential value to leak in the first place.
func (h *Handlers) List(w http.ResponseWriter, _ *http.Request) {
	schemas := h.Registry.Schemas()
	resp := kindsResponse{Kinds: make([]kindDTO, 0, len(schemas))}
	for _, s := range schemas {
		fields := make([]fieldDTO, 0, len(s.Fields))
		for _, f := range s.Fields {
			fields = append(fields, fieldDTO(f))
		}
		resp.Kinds = append(resp.Kinds, kindDTO{Kind: s.Kind, Label: s.Label, Fields: fields})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
