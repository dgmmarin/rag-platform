package connector_test

import (
	"encoding/json"
	"testing"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/connector/api"
	"github.com/rag-platform/ragctl/internal/connector/upload"
	"github.com/rag-platform/ragctl/internal/connector/webcrawl"
)

// TestConnectorKindsDriftGuard (SPEC-11 §10, STORY-11.2, ADR-0075) is the
// drift-guard: for each real, registered connector, take a config baseline that
// satisfies its ValidateConfig, then for every field its Fields() marks
// Required:true, remove exactly that key from the baseline and assert
// ValidateConfig now rejects it. A FieldSpec claiming Required:true that
// ValidateConfig does not actually enforce — the rendered form and the server
// would silently drift — fails this test.
//
// ponytail: this only guards ONE direction (a claimed-required field that isn't
// enforced). It does not detect the opposite drift — a field ValidateConfig
// truly requires but that is missing from Fields() entirely, or wrongly marked
// Required:false — since that would need re-deriving each JSON Schema's
// `required` list independently of Fields() itself. Upgrade path: assert each
// configSchema's `required` array against Fields() directly if that direction of
// drift is ever hit in practice.
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
			if len(fields) == 0 && len(tc.baseline) == 0 {
				return // e.g. upload: nothing required, nothing to drift-guard
			}

			required := 0
			for _, f := range fields {
				if !f.Required {
					continue
				}
				required++
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
					t.Errorf("field %q is marked Required but ValidateConfig accepted a config without it (drift): cfg=%s", f.Name, vb)
				}
			}
			if required == 0 {
				t.Fatalf("%s: Fields() has no Required:true field but the baseline config is non-empty; nothing drift-guarded", tc.name)
			}
		})
	}
}
