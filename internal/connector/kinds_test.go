package connector_test

import (
	"encoding/json"
	"testing"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/connector/api"
	"github.com/rag-platform/ragctl/internal/connector/upload"
	"github.com/rag-platform/ragctl/internal/connector/webcrawl"
)

// requiredFieldser is implemented by each real connector via RequiredConfigFields
// — a small, deliberately separate accessor (NOT part of connector.Connector)
// exposing its own JSON-Schema top-level `required` array, the input to the
// reverse-direction drift guard below.
type requiredFieldser interface {
	RequiredConfigFields() []string
}

// TestConnectorKindsDriftGuard (SPEC-11 §10, STORY-11.2, ADR-0075) is the
// drift-guard, checked in BOTH directions so a connector's Fields() and its
// ValidateConfig schema can never silently point at two different truths:
//
//  1. Forward: take a config baseline that satisfies ValidateConfig, then for
//     every field Fields() marks Required:true, remove exactly that key from the
//     baseline and assert ValidateConfig now rejects it. A FieldSpec claiming
//     Required:true that ValidateConfig does not actually enforce fails this.
//  2. Reverse (the more harmful direction for the schema-driven admin UI, SPEC-11
//     §10.1: a form that omits a truly-required field lets create fail
//     server-side with no client-side signal): every key in the connector's own
//     JSON-Schema top-level `required` array (RequiredConfigFields) must appear
//     in Fields() marked Required:true. Deliberately top-level only — the `api`
//     connector's nested `auth`/`endpoints` object schemas have their own nested
//     `required` (e.g. auth.type), which is not something a flat FieldSpec list
//     represents or needs to.
func TestConnectorKindsDriftGuard(t *testing.T) {
	cases := []struct {
		name     string
		conn     connector.Connector
		baseline map[string]any
	}{
		{
			name:     "upload",
			conn:     upload.New(),
			baseline: map[string]any{},
		},
		{
			name: "web_crawl",
			conn: webcrawl.New(),
			baseline: map[string]any{
				"start_urls": []string{"https://example.com"},
			},
		},
		{
			name: "sitemap",
			conn: webcrawl.NewSitemap(),
			baseline: map[string]any{
				"sitemap_urls": []string{"https://example.com/sitemap.xml"},
			},
		},
		{
			name: "api",
			conn: api.New(),
			baseline: map[string]any{
				"base_url": "https://api.example.com",
				"auth":     map[string]any{"type": "bearer"},
				"endpoints": []map[string]any{{
					"name":       "items",
					"path":       "/items",
					"pagination": map[string]any{"type": "none"},
					"items_path": "$.items",
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, err := json.Marshal(tc.baseline)
			if err != nil {
				t.Fatalf("marshal baseline: %v", err)
			}
			if err := tc.conn.ValidateConfig(base); err != nil {
				t.Fatalf("baseline config rejected (fix the test's baseline, not the connector): %v; cfg=%s", err, base)
			}

			fields := tc.conn.Fields()
			declaredRequired := make(map[string]bool, len(fields))
			required := 0
			for _, f := range fields {
				if !f.Required {
					continue
				}
				declaredRequired[f.Name] = true
				required++
				if len(tc.baseline) == 0 {
					continue // nothing in the baseline to omit (e.g. upload)
				}
				variant := make(map[string]any, len(tc.baseline))
				for k, v := range tc.baseline {
					if k == f.Name {
						continue
					}
					variant[k] = v
				}
				vb, err := json.Marshal(variant)
				if err != nil {
					t.Fatalf("marshal variant omitting %q: %v", f.Name, err)
				}
				if err := tc.conn.ValidateConfig(vb); err == nil {
					t.Errorf("field %q is marked Required but ValidateConfig accepted a config without it (forward drift): cfg=%s", f.Name, vb)
				}
			}
			if required == 0 && len(tc.baseline) != 0 {
				t.Fatalf("%s: Fields() has no Required:true field but the baseline config is non-empty; nothing forward-drift-guarded", tc.name)
			}

			// Reverse: every schema-required key must be declared Required:true.
			rf, ok := tc.conn.(requiredFieldser)
			if !ok {
				t.Fatalf("%s: connector does not implement RequiredConfigFields; reverse drift guard cannot run", tc.name)
			}
			for _, key := range rf.RequiredConfigFields() {
				if !declaredRequired[key] {
					t.Errorf("schema requires %q but Fields() does not declare it Required:true (reverse drift): the admin UI would render a form that can never satisfy this field, so create would fail server-side with no client-side signal", key)
				}
			}

			// Type drift: a field whose config value is an ARRAY or OBJECT must NOT
			// carry a scalar input type — otherwise the schema-driven form submits a
			// string the connector rejects ("got string, want array", ISSUE-0061).
			// The baseline carries a representative value per required key, so its Go
			// kind is the source of truth for the field's config shape.
			specByName := make(map[string]connector.FieldSpec, len(fields))
			for _, f := range fields {
				specByName[f.Name] = f
			}
			for key, val := range tc.baseline {
				f, ok := specByName[key]
				if !ok {
					continue // baseline may carry a key with no rendered field
				}
				switch val.(type) {
				case []string, []any, []map[string]any:
					if f.Type != "stringlist" && f.Type != "json" {
						t.Errorf("field %q holds a JSON array but its FieldSpec Type is %q (want \"stringlist\" or \"json\"); the form would submit a scalar string", key, f.Type)
					}
				case map[string]any:
					if f.Type != "json" {
						t.Errorf("field %q holds a JSON object but its FieldSpec Type is %q (want \"json\"); the form would submit a scalar string", key, f.Type)
					}
				}
			}
		})
	}
}
