package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// jsonItem decodes a JSON object into the exact shape Sync hands the mapper: a
// map[string]any decoded with UseNumber (so numbers are json.Number), which is what
// the paginator produces from a real response.
func jsonItem(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode item: %v", err)
	}
	return v
}

// TestBuildDocumentTemplate is the SPEC-04 §4 golden mapping: template → body,
// uri_template → URI, metadata JSONPath → Metadata, id_path → ExternalID,
// updated_path → ModifiedAt.
func TestBuildDocumentTemplate(t *testing.T) {
	ep := endpoint{
		Name:        "products",
		IDPath:      "$.id",
		UpdatedPath: "$.updated_at",
		Template:    "# {{.name}}\nSKU: {{.sku}}\nPrice: {{money .price .currency}}\n\n{{.description}}",
		URITemplate: "https://acme.com/p/{{.slug}}",
		Metadata:    map[string]string{"category": "$.category.name", "sku": "$.sku"},
	}
	m, err := newDocMapper(ep)
	if err != nil {
		t.Fatalf("newDocMapper: %v", err)
	}
	item := jsonItem(t, `{
		"id":"p-42","slug":"widget","name":"Widget","sku":"W-1","price":19.5,"currency":"USD",
		"description":"A fine widget.","category":{"name":"Tools"},"updated_at":"2026-01-02T03:04:05Z"
	}`)
	res, err := m.build(item, 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	wantText := "# Widget\nSKU: W-1\nPrice: 19.50 USD\n\nA fine widget."
	if res.doc.Text != wantText {
		t.Fatalf("body text:\n got %q\nwant %q", res.doc.Text, wantText)
	}
	if res.doc.MimeType != "text/markdown" {
		t.Fatalf("mime = %q, want text/markdown", res.doc.MimeType)
	}
	if res.doc.ExternalID != "products/p-42" {
		t.Fatalf("ExternalID = %q, want products/p-42", res.doc.ExternalID)
	}
	if res.doc.URI != "https://acme.com/p/widget" {
		t.Fatalf("URI = %q", res.doc.URI)
	}
	if got := res.doc.Metadata["category"]; got != "Tools" {
		t.Fatalf("metadata category = %v, want Tools", got)
	}
	if got := res.doc.Metadata["sku"]; got != "W-1" {
		t.Fatalf("metadata sku = %v, want W-1", got)
	}
	if res.doc.ModifiedAt == nil || !res.doc.ModifiedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("ModifiedAt = %v, want 2026-01-02T03:04:05Z", res.doc.ModifiedAt)
	}
	if !res.hasUpdated || res.updatedRaw != "2026-01-02T03:04:05Z" {
		t.Fatalf("updatedRaw = %q hasUpdated=%v, want verbatim source value", res.updatedRaw, res.hasUpdated)
	}
}

// TestBuildDocumentNoTemplateFallsBackToRawJSON keeps the 07.6 behaviour when no
// template is configured (so the auth×pagination matrix stays green): raw JSON body.
func TestBuildDocumentNoTemplateFallsBackToRawJSON(t *testing.T) {
	m, err := newDocMapper(endpoint{Name: "items", IDPath: "$.id"})
	if err != nil {
		t.Fatalf("newDocMapper: %v", err)
	}
	res, err := m.build(jsonItem(t, `{"id":"i1","name":"x"}`), 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.doc.MimeType != "application/json" || res.doc.Text == "" {
		t.Fatalf("fallback doc should be raw JSON; mime=%q text=%q", res.doc.MimeType, res.doc.Text)
	}
	if res.doc.ExternalID != "items/i1" {
		t.Fatalf("ExternalID = %q", res.doc.ExternalID)
	}
}

// TestBuildDocumentMissingIDUsesSeq mirrors the 07.6 placeholder fallback: an item
// without a resolvable id_path is namespaced by its sequence number.
func TestBuildDocumentMissingIDUsesSeq(t *testing.T) {
	m, _ := newDocMapper(endpoint{Name: "items", IDPath: "$.id"})
	res, err := m.build(jsonItem(t, `{"name":"no id here"}`), 7)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.doc.ExternalID != "items/7" {
		t.Fatalf("ExternalID = %q, want items/7", res.doc.ExternalID)
	}
}

// TestMissingFieldRendersNoValue pins the documented missing-key behaviour: the
// text/template default renders a missing field as "<no value>" (ADR-0049). It is
// predictable and visible rather than a silent empty string.
func TestMissingFieldRendersNoValue(t *testing.T) {
	m, err := newDocMapper(endpoint{Name: "e", Template: "name={{.name}} missing={{.nope}}"})
	if err != nil {
		t.Fatalf("newDocMapper: %v", err)
	}
	res, err := m.build(jsonItem(t, `{"name":"here"}`), 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.doc.Text != "name=here missing=<no value>" {
		t.Fatalf("body = %q, want the missing field rendered as <no value>", res.doc.Text)
	}
}

// TestTemplateExecutionErrorReturned proves an execution error (navigating into a
// scalar) is returned so Sync can record-and-skip the item, never aborting the sync.
func TestTemplateExecutionErrorReturned(t *testing.T) {
	m, err := newDocMapper(endpoint{Name: "e", Template: "{{.name.deep}}"})
	if err != nil {
		t.Fatalf("newDocMapper: %v", err)
	}
	if _, err := m.build(jsonItem(t, `{"name":"scalar"}`), 0); err == nil {
		t.Fatal("expected a template execution error navigating into a scalar")
	}
}

// TestBadTemplateParseRejected proves a malformed template is a compile-time error
// (a config problem for every item), surfaced by newDocMapper, not per item.
func TestBadTemplateParseRejected(t *testing.T) {
	if _, err := newDocMapper(endpoint{Name: "e", Template: "{{.name"}); err == nil {
		t.Fatal("expected a parse error for a malformed template")
	}
	if _, err := newDocMapper(endpoint{Name: "e", URITemplate: "{{.slug"}); err == nil {
		t.Fatal("expected a parse error for a malformed uri_template")
	}
}

func TestHelperJoin(t *testing.T) {
	m, _ := newDocMapper(endpoint{Name: "e", Template: "{{join .tags \", \"}}"})
	res, err := m.build(jsonItem(t, `{"tags":["a","b","c"]}`), 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.doc.Text != "a, b, c" {
		t.Fatalf("join = %q, want 'a, b, c'", res.doc.Text)
	}
	// A missing / null list joins to the empty string, not an error.
	res2, err := m.build(jsonItem(t, `{}`), 0)
	if err != nil {
		t.Fatalf("build empty: %v", err)
	}
	if res2.doc.Text != "" {
		t.Fatalf("join of missing list = %q, want empty", res2.doc.Text)
	}
}

func TestHelperMoney(t *testing.T) {
	cases := []struct {
		tmpl string
		item string
		want string
	}{
		{`{{money .price .cur}}`, `{"price":19.5,"cur":"USD"}`, "19.50 USD"},
		{`{{money .price}}`, `{"price":1000}`, "1000.00"},
		{`{{money .price .cur}}`, `{"price":"42.1","cur":"EUR"}`, "42.10 EUR"},
		{`{{money .price .cur}}`, `{"price":5,"cur":""}`, "5.00"},
	}
	for _, c := range cases {
		m, err := newDocMapper(endpoint{Name: "e", Template: c.tmpl})
		if err != nil {
			t.Fatalf("newDocMapper(%q): %v", c.tmpl, err)
		}
		res, err := m.build(jsonItem(t, c.item), 0)
		if err != nil {
			t.Fatalf("build(%q): %v", c.item, err)
		}
		if res.doc.Text != c.want {
			t.Fatalf("money(%s over %s) = %q, want %q", c.tmpl, c.item, res.doc.Text, c.want)
		}
	}
}

func TestHelperDate(t *testing.T) {
	cases := []struct {
		tmpl string
		item string
		want string
	}{
		{`{{date .t "2006-01-02"}}`, `{"t":"2026-01-02T03:04:05Z"}`, "2026-01-02"},
		{`{{date .t "2006"}}`, `{"t":"2026-01-02"}`, "2026"},
		{`{{date .t "2006-01-02"}}`, `{"t":1767322445}`, "2026-01-02"}, // epoch seconds
	}
	for _, c := range cases {
		m, err := newDocMapper(endpoint{Name: "e", Template: c.tmpl})
		if err != nil {
			t.Fatalf("newDocMapper(%q): %v", c.tmpl, err)
		}
		res, err := m.build(jsonItem(t, c.item), 0)
		if err != nil {
			t.Fatalf("build(%q): %v", c.item, err)
		}
		if res.doc.Text != c.want {
			t.Fatalf("date(%s over %s) = %q, want %q", c.tmpl, c.item, res.doc.Text, c.want)
		}
	}
	// An unparseable value is returned verbatim (predictable, never an error).
	m, _ := newDocMapper(endpoint{Name: "e", Template: `{{date .t "2006"}}`})
	res, err := m.build(jsonItem(t, `{"t":"not a date"}`), 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.doc.Text != "not a date" {
		t.Fatalf("date of unparseable = %q, want verbatim", res.doc.Text)
	}
}

// TestUpdatedPathParsing pins ModifiedAt parsing across the accepted layouts.
func TestUpdatedPathParsing(t *testing.T) {
	m, _ := newDocMapper(endpoint{Name: "e", UpdatedPath: "$.u"})
	for _, in := range []string{"2026-01-02T03:04:05Z", "2026-01-02"} {
		res, err := m.build(jsonItem(t, `{"u":"`+in+`"}`), 0)
		if err != nil {
			t.Fatalf("build(%q): %v", in, err)
		}
		if res.doc.ModifiedAt == nil {
			t.Fatalf("ModifiedAt nil for %q", in)
		}
	}
	// No updated_path value -> no ModifiedAt, hasUpdated false.
	res, err := m.build(jsonItem(t, `{}`), 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.doc.ModifiedAt != nil || res.hasUpdated {
		t.Fatalf("expected no ModifiedAt when updated_path absent")
	}
}
