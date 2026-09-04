package api

import (
	"encoding/json"
	"strconv"
	"strings"
)

// evalPath evaluates a simple JSONPath-style dot path against a decoded JSON value
// (SPEC-04 §4). The SPEC uses only a tiny grammar — a leading "$" root followed by
// dot-separated object keys ("$.data", "$.category.name") and optional numeric array
// indices ("$.items.0") — so this is a deliberately minimal evaluator, NOT a full
// JSONPath (no wildcards, filters, unions or recursion, none of which appear in the
// SPEC). Keeping it hand-rolled avoids a JSONPath dependency for paths this simple;
// ADR-0048 records the decision. "$" or "" selects the root itself.
//
// STORY-07.6 uses it for pagination (items_path, cursor_path) and the placeholder
// id_path; STORY-07.7 reuses the identical evaluator for metadata extraction.
func evalPath(root any, path string) (any, bool) {
	segs, ok := parsePath(path)
	if !ok {
		return nil, false
	}
	cur := root
	for _, s := range segs {
		switch node := cur.(type) {
		case map[string]any:
			v, exists := node[s]
			if !exists {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			// Cannot descend into a scalar.
			return nil, false
		}
	}
	return cur, true
}

// parsePath splits a dot path into its segments. A leading "$" and/or "." is
// stripped; "$"/"" yields the empty segment list (the root). A malformed path with
// an empty interior segment (e.g. "$..x") is rejected.
func parsePath(path string) ([]string, bool) {
	p := strings.TrimSpace(path)
	p = strings.TrimPrefix(p, "$")
	p = strings.TrimPrefix(p, ".")
	if p == "" {
		return nil, true // root selector
	}
	segs := strings.Split(p, ".")
	for _, s := range segs {
		if s == "" {
			return nil, false
		}
	}
	return segs, true
}

// evalItems resolves path to a JSON array and returns its elements. It is how the
// paginator enumerates a page's records (items_path). A path that is absent or
// resolves to a non-array is not enumerable.
func evalItems(root any, path string) ([]any, bool) {
	v, ok := evalPath(root, path)
	if !ok {
		return nil, false
	}
	arr, isArr := v.([]any)
	if !isArr {
		return nil, false
	}
	return arr, true
}

// evalString resolves path to a scalar and renders it as a string — used for the
// next cursor (cursor_path) and the placeholder id (id_path). A string is returned
// verbatim; a json.Number keeps its exact text (so an integer cursor is "12345",
// never "1.2345e+04"); a bool renders as true/false. JSON null, an absent path, and
// a non-scalar (object/array) all read as "no value" (ok=false), so an empty or
// missing cursor cleanly terminates pagination.
func evalString(root any, path string) (string, bool) {
	v, ok := evalPath(root, path)
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	default:
		// Object or array: not a scalar value.
		return "", false
	}
}
