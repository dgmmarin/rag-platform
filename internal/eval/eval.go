// Package eval is the per-tenant evaluation-harness case store (FR-ADM-04,
// STORY-12.1). Eval cases are question/expected-answer pairs used to measure
// retrieval and answer quality; they are TENANT content (schemas/tenant.sql
// eval_cases), never control-plane data (C-3). Every read and write reaches the
// tenant database ONLY through a *tenant.DB obtained from the Resolver (ADR-0003)
// — there is no tenant_id column and no control-plane path to this data. The
// tenant is always the one resolved from the operator's chosen slug via the
// resolver, never a value smuggled into a row.
//
// This story delivers CRUD plus CSV import (the authoring side). Running the
// cases (recall@k, grounded rate, latency; eval_runs/eval_results) is STORY-12.2
// and lives elsewhere.
package eval

import (
	"fmt"
	"strings"
	"time"
)

// listSeparator encodes multi-valued CSV columns (expected_doc_ids, tags) inside
// a single cell. `|` is chosen over comma so the values survive a spreadsheet
// round trip without extra quoting, and over whitespace so tags may contain
// spaces (ADR-0069).
const listSeparator = "|"

// maxListCases caps how many cases List returns in one call. Eval sets are
// human-authored and small (tens, maybe low hundreds per tenant), so a single
// ordered read is enough.
//
// ponytail: no keyset pagination — the whole (capped) set is returned. Ceiling:
// a tenant with more than maxListCases cases sees only the newest maxListCases.
// Upgrade path: add a keyset cursor like internal/documents if that ever bites.
const maxListCases = 1000

// Case is a stored evaluation case (schemas/tenant.sql eval_cases).
type Case struct {
	ID             string    `json:"id"`
	Question       string    `json:"question"`
	ExpectedAnswer *string   `json:"expected_answer,omitempty"`
	ExpectedDocIDs []string  `json:"expected_doc_ids"`
	Tags           []string  `json:"tags"`
	CreatedAt      time.Time `json:"created_at"`
}

// CaseInput is the validated write shape for Create, Update and CSV import. A
// non-empty ID makes an import row an upsert (update that case, or insert it with
// that id); an empty ID creates a case with a generated id.
type CaseInput struct {
	ID             string
	Question       string
	ExpectedAnswer *string
	ExpectedDocIDs []string
	Tags           []string
}

// ImportResult reports the outcome of a CSV import.
type ImportResult struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
}

// ValidationError is a client-safe 400-style error (bad CSV header, empty
// question, malformed UUID). Callers match with errors.As.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) *ValidationError {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// validate enforces the trust-boundary invariants for a write: a non-empty
// question, well-formed UUIDs in id and expected_doc_ids, and non-empty tags.
func (in CaseInput) validate() error {
	if strings.TrimSpace(in.Question) == "" {
		return invalid("question must not be empty")
	}
	if in.ID != "" && !validUUID(in.ID) {
		return invalid("id %q is not a valid UUID", in.ID)
	}
	for _, id := range in.ExpectedDocIDs {
		if !validUUID(id) {
			return invalid("expected_doc_id %q is not a valid UUID", id)
		}
	}
	for _, tag := range in.Tags {
		if strings.TrimSpace(tag) == "" {
			return invalid("tags must not contain empty values")
		}
	}
	return nil
}

// splitList splits a `|`-separated list cell into trimmed, non-empty items. An
// empty or all-whitespace cell yields no items (nil), so an absent column and an
// empty cell are treated the same.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, listSeparator) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validUUID is a cheap canonical-form check (mirrors internal/documents): it
// keeps a malformed value out of the tenant DB (where uuid[] casting would raise
// a Postgres error) and turns a non-UUID lookup id into a clean not-found.
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
