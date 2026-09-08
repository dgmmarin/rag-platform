package webcrawl

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/egress"
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

// Fields returns the sitemap form-field descriptors (SPEC-11 §10). sitemap_urls is
// the only field sitemapConfigSchema's `required` names, so it is the only one
// marked Required — the drift guard (internal/connector/kinds_test.go) checks that
// omitting it fails ValidateConfig. A sitemap authenticates nothing (Test below),
// so there is no secret field here.
func (sitemapConnector) Fields() []connector.FieldSpec {
	return []connector.FieldSpec{
		{Name: "sitemap_urls", Label: "Sitemap URLs", Type: "text", Required: true},
		{Name: "max_pages", Label: "Max Pages", Type: "number", Required: false},
		{Name: "delay_ms", Label: "Delay (ms)", Type: "number", Required: false},
		{Name: "concurrency", Label: "Concurrency", Type: "number", Required: false},
	}
}

// RequiredConfigFields exposes sitemapConfigSchema's own top-level `required` keys
// (here: sitemap_urls) for the reverse drift guard (internal/connector/
// kinds_test.go, SPEC-11 §10) — NOT part of connector.Connector, a test-only
// introspection hook.
func (sitemapConnector) RequiredConfigFields() []string { return sitemapConfigSchema.Required() }

// Test validates the config, then fetches AND parses the first sitemap URL through
// the SSRF-guarded egress Doer, bounded by the ≤10 s probe deadline (FR-SRC-14,
// STORY-07.8). It reuses the STORY-07.5 sitemap fetch (gzip + size cap) and parser.
// Outcomes map to actionable, secret-free errors: unreachable (host not found /
// address not permitted / refused / timed out), a non-2xx status, a non-XML body, or
// an empty sitemap (no <url>/<sitemap> entries). A sitemap authenticates nothing, so
// creds is ignored.
func (sitemapConnector) Test(ctx context.Context, cfg json.RawMessage, _ connector.Credentials) error {
	if err := (sitemapConnector{}).ValidateConfig(cfg); err != nil {
		return err
	}
	var c config
	if err := json.Unmarshal(cfg, &c); err != nil {
		return err // unreachable after ValidateConfig
	}
	_, u, err := parseAndNormalize(c.SitemapURLs[0])
	if err != nil {
		return err // unreachable after ValidateConfig
	}
	ctx, cancel := context.WithTimeout(ctx, egress.ProbeTimeout)
	defer cancel()

	cr := newSitemapCrawler(c, syncDoer)
	body, err := cr.fetchSitemap(ctx, u.String())
	if err != nil {
		if msg, ok := transportMessage(err, u.Host); ok {
			return fmt.Errorf("sitemap: %s", msg)
		}
		// A non-2xx status / size-cap failure: fetchSitemap's message carries no
		// secret (a sitemap URL has no credentials), so it is safe to surface.
		return fmt.Errorf("sitemap: could not fetch sitemap: %v", err)
	}
	var f sitemapFile
	if err := xml.Unmarshal(body, &f); err != nil {
		return errors.New("sitemap: response is not valid XML")
	}
	if len(f.URLs) == 0 && len(f.Sitemaps) == 0 {
		return errors.New("sitemap: no <url> or <sitemap> entries found (empty sitemap)")
	}
	return nil
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
