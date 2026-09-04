package webcrawl

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rag-platform/ragctl/internal/connector"
)

// sitemapConfigSchema is the SPEC-04 §3 sitemap-connector config contract. It mirrors
// the web-crawl schema for the shared politeness/extraction knobs but seeds the crawl
// from sitemap_urls (required, non-empty) instead of start_urls, and has no max_depth
// or render_js (no link following). additionalProperties:false rejects a stray
// web-crawl-only key (e.g. start_urls) so a mis-kinded config fails loudly.
var sitemapConfigSchema = connector.MustSchemaValidator([]byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["sitemap_urls"],
  "properties": {
    "sitemap_urls":      {"type": "array", "minItems": 1, "items": {"type": "string"}},
    "allow":             {"type": "array", "items": {"type": "string"}},
    "deny":              {"type": "array", "items": {"type": "string"}},
    "max_pages":         {"type": "integer", "minimum": 0},
    "delay_ms":          {"type": "integer", "minimum": 0},
    "concurrency":       {"type": "integer", "minimum": 0},
    "include_selectors": {"type": "array", "items": {"type": "string"}},
    "exclude_selectors": {"type": "array", "items": {"type": "string"}}
  }
}`))

// sitemapConnector implements connector.Connector for KindSitemap (SPEC-04 §3). It
// lives in the webcrawl package and drives the SAME crawl core as web_crawl (fetch,
// egress/SSRF guard, conditional GET/content-hash change detection, HTML→markdown
// extraction, canonical de-dup, crawl_pages state), differing only in the three ways
// SPEC-04 §3 calls out: the frontier is seeded from sitemap XML, links are not
// followed, and <lastmod> drives incremental skipping.
type sitemapConnector struct{}

// NewSitemap returns a fresh sitemap connector.
func NewSitemap() connector.Connector { return sitemapConnector{} }

func (sitemapConnector) Kind() connector.Kind { return connector.KindSitemap }

// ValidateConfig checks the config against the JSON schema and then that every
// sitemap_url is an absolute http(s) URL.
func (sitemapConnector) ValidateConfig(cfg json.RawMessage) error {
	if err := sitemapConfigSchema.Validate(cfg); err != nil {
		return err
	}
	var c config
	if err := json.Unmarshal(cfg, &c); err != nil {
		return &connector.ConfigError{Fields: []connector.FieldError{{Message: "config must be valid JSON"}}}
	}
	return validateSitemapSemantics(c)
}

func validateSitemapSemantics(c config) error {
	for i, raw := range c.SitemapURLs {
		if _, _, err := parseAndNormalize(raw); err != nil {
			return &connector.ConfigError{Fields: []connector.FieldError{{
				Field:   fmt.Sprintf("sitemap_urls.%d", i),
				Message: "must be an absolute http(s) URL",
			}}}
		}
	}
	return nil
}

// Test validates the configuration without network I/O (FR-SRC-14). Live reachability
// probing across all connector kinds is STORY-07.8; here Test guarantees the config is
// well-formed so an obviously invalid source is rejected before it is scheduled.
func (sitemapConnector) Test(_ context.Context, cfg json.RawMessage, _ connector.Credentials) error {
	return sitemapConnector{}.ValidateConfig(cfg)
}

// Sync enumerates the sitemap(s) into the sink (SPEC-04 §3). It decodes the config,
// builds the shared crawl core with link following disabled, and runs it with the
// frontier seeded from the parsed sitemap XML.
func (sitemapConnector) Sync(ctx context.Context, run connector.SyncRun, sink connector.Sink) (connector.Stats, error) {
	var c config
	if err := json.Unmarshal(run.Config, &c); err != nil {
		return connector.Stats{}, fmt.Errorf("sitemap: decode config: %w", err)
	}
	if err := validateSitemapSemantics(c); err != nil {
		return connector.Stats{}, err
	}
	cr := newSitemapCrawler(c, syncDoer)
	return cr.runSitemap(ctx, c.SitemapURLs, run, sink)
}

func init() {
	connector.Register(connector.KindSitemap, NewSitemap)
}
