package auth

import "fmt"

// Scope is an API-key capability. The set is fixed by SPEC-07 §2 / FR-ACC-04
// (query, ingest, admin); no other scope is accepted, so a key can never be
// minted with an invented capability. The zero value is invalid and grants
// nothing, so a mis-scanned scope fails closed.
type Scope string

// The three API-key scopes defined by SPEC-07 §2 / FR-ACC-04.
const (
	ScopeQuery  Scope = "query"  // /v1/query, /v1/retrieve, reads
	ScopeIngest Scope = "ingest" // POST/DELETE /v1/documents
	ScopeAdmin  Scope = "admin"  // sources, jobs, members, settings, api-keys
)

// validScopes is the SPEC-07 §2 set encoded once; membership is the definition
// of "valid". Absence means "unknown scope, rejected".
var validScopes = map[Scope]bool{
	ScopeQuery:  true,
	ScopeIngest: true,
	ScopeAdmin:  true,
}

// Valid reports whether s is one of the three spec scopes.
func (s Scope) Valid() bool { return validScopes[s] }

// ParseScope validates untrusted scope text (from an API payload or a DB scan)
// and returns the typed Scope, rejecting anything outside the spec set.
func ParseScope(s string) (Scope, error) {
	sc := Scope(s)
	if !sc.Valid() {
		return "", fmt.Errorf("auth: invalid scope %q", s)
	}
	return sc, nil
}

// ScopeSet is the set of scopes granted to a key, used for O(1) enforcement.
type ScopeSet map[Scope]bool

// ParseScopes validates a list of scope strings and returns them as a set. A key
// must carry at least one scope; an empty or nil list is rejected so no
// capability-less key is created or authenticates. Duplicates collapse.
func ParseScopes(ss []string) (ScopeSet, error) {
	if len(ss) == 0 {
		return nil, fmt.Errorf("auth: an API key must have at least one scope")
	}
	set := make(ScopeSet, len(ss))
	for _, s := range ss {
		sc, err := ParseScope(s)
		if err != nil {
			return nil, err
		}
		set[sc] = true
	}
	return set, nil
}

// Has reports whether the set grants the scope. A request keyed to a scope the
// presented key lacks is refused (FR-ACC-04).
func (s ScopeSet) Has(scope Scope) bool { return s[scope] }

// Slice returns the granted scopes as strings for storage/display, in no
// guaranteed order.
func (s ScopeSet) Slice() []string {
	out := make([]string, 0, len(s))
	for sc := range s {
		out = append(out, string(sc))
	}
	return out
}
