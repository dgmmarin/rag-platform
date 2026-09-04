package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeAny decodes JSON with UseNumber so numeric cursors/ids keep an exact
// string form (no float64 rounding), matching how the connector reads responses.
func decodeAny(t *testing.T, s string) any {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return v
}

func TestEvalPathObjectAndNested(t *testing.T) {
	root := decodeAny(t, `{"id":"p1","category":{"name":"Widgets"},"data":[1,2,3]}`)

	if got, ok := evalPath(root, "$.id"); !ok || got != "p1" {
		t.Fatalf("$.id = %v, %v; want p1", got, ok)
	}
	if got, ok := evalPath(root, "$.category.name"); !ok || got != "Widgets" {
		t.Fatalf("$.category.name = %v, %v; want Widgets", got, ok)
	}
	if _, ok := evalPath(root, "$.missing"); ok {
		t.Fatalf("$.missing should be absent")
	}
	if _, ok := evalPath(root, "$.category.missing"); ok {
		t.Fatalf("$.category.missing should be absent")
	}
}

func TestEvalPathRootSelectsWhole(t *testing.T) {
	root := decodeAny(t, `[{"id":"a"},{"id":"b"}]`)
	got, ok := evalPath(root, "$")
	if !ok {
		t.Fatalf("$ should select the root")
	}
	arr, isArr := got.([]any)
	if !isArr || len(arr) != 2 {
		t.Fatalf("$ = %v; want the 2-element root array", got)
	}
	// Empty path is equivalent to "$".
	if _, ok := evalPath(root, ""); !ok {
		t.Fatalf("empty path should select the root")
	}
}

func TestEvalPathArrayIndex(t *testing.T) {
	root := decodeAny(t, `{"items":[{"id":"x"},{"id":"y"}]}`)
	got, ok := evalPath(root, "$.items.1.id")
	if !ok || got != "y" {
		t.Fatalf("$.items.1.id = %v, %v; want y", got, ok)
	}
	if _, ok := evalPath(root, "$.items.9.id"); ok {
		t.Fatalf("out-of-range index should be absent")
	}
}

func TestEvalItems(t *testing.T) {
	root := decodeAny(t, `{"data":[{"id":"1"},{"id":"2"}]}`)
	items, ok := evalItems(root, "$.data")
	if !ok || len(items) != 2 {
		t.Fatalf("evalItems($.data) = %v, %v; want 2 items", items, ok)
	}

	// A top-level array with an empty items_path is enumerated directly.
	rootArr := decodeAny(t, `[{"id":"1"},{"id":"2"},{"id":"3"}]`)
	items, ok = evalItems(rootArr, "")
	if !ok || len(items) != 3 {
		t.Fatalf("evalItems(root array, \"\") = %v, %v; want 3 items", items, ok)
	}

	// A path that resolves to a non-array is not enumerable.
	if _, ok := evalItems(root, "$.data.0.id"); ok {
		t.Fatalf("a scalar path should not yield items")
	}
}

func TestEvalStringNumberAndString(t *testing.T) {
	root := decodeAny(t, `{"next_cursor":"abc123","page":42,"missing":null}`)

	if got, ok := evalString(root, "$.next_cursor"); !ok || got != "abc123" {
		t.Fatalf("$.next_cursor = %q, %v; want abc123", got, ok)
	}
	// A numeric cursor keeps its exact integer text (no 4.2e+01).
	if got, ok := evalString(root, "$.page"); !ok || got != "42" {
		t.Fatalf("$.page = %q, %v; want 42", got, ok)
	}
	// JSON null and absent both read as "no value".
	if _, ok := evalString(root, "$.missing"); ok {
		t.Fatalf("$.missing (null) should read as absent")
	}
	if _, ok := evalString(root, "$.nope"); ok {
		t.Fatalf("$.nope should read as absent")
	}
	// An empty path yields no scalar (the root is an object here).
	if _, ok := evalString(root, "$.next_cursor.x"); ok {
		t.Fatalf("descending into a string should be absent")
	}
}
