// Package webcrawl is the "web_crawl" source connector (SPEC-04 §2, FR-SRC-03/04):
// a breadth-first crawler that enumerates a website within an allowlist, honours
// robots.txt and a per-host politeness delay, normalises and de-duplicates URLs,
// follows <link rel=canonical>, and streams each fetched page into the ingestion
// Sink as a connector.Document. Crawl state is persisted to the tenant crawl_pages
// table so an interrupted crawl resumes rather than restarting (STORY-07.1).
//
// Registration happens in this package's init(), so a blank import
// (`_ ".../internal/connector/webcrawl"`) at the composition root wires it with no
// other change (NFR-MNT-01), exactly like the upload connector.
//
// Story boundaries (seams left for the rest of EPIC-07):
//   - EGRESS/SSRF (STORY-07.2): all fetches go through the injectable Doer egress
//     seam (egress.go). The default permits loopback so httptest-based tests work;
//     07.2 drops an SSRF-guarded transport in with no crawl-logic change.
//   - EXTRACTION (STORY-07.3): pages are emitted as RAW bytes (Body); only
//     title/canonical/links are parsed here (extract.go). 07.3 adds readability +
//     include/exclude selectors behind the same parse step.
//   - CONDITIONAL FETCH (STORY-07.4): etag/last_modified/content_hash ARE persisted
//     to crawl_pages, but no If-None-Match/HEAD/304-skip optimisation is done here.
package webcrawl

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rag-platform/ragctl/internal/connector"
)

// userAgent identifies the platform crawler and carries a contact URL, as required
// by SPEC-04 §2 ("User-Agent identifies the platform and a contact URL"), so a site
// operator can see who is crawling and how to reach us.
const userAgent = "RAGPlatformCrawler/1.0 (+https://rag-platform.example/bot)"

// configSchema is the SPEC-04 §2 web-crawl config contract. start_urls is required
// and non-empty; unknown keys are rejected so a typo fails loudly. Semantic checks
// the schema cannot express (URL scheme, render_js support) are in ValidateConfig.
var configSchema = connector.MustSchemaValidator([]byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["start_urls"],
  "properties": {
    "start_urls":        {"type": "array", "minItems": 1, "items": {"type": "string"}},
    "allow":             {"type": "array", "items": {"type": "string"}},
    "deny":              {"type": "array", "items": {"type": "string"}},
    "max_depth":         {"type": "integer", "minimum": 0},
    "max_pages":         {"type": "integer", "minimum": 0},
    "delay_ms":          {"type": "integer", "minimum": 0},
    "concurrency":       {"type": "integer", "minimum": 0},
    "include_selectors": {"type": "array", "items": {"type": "string"}},
    "exclude_selectors": {"type": "array", "items": {"type": "string"}},
    "render_js":         {"type": "boolean"}
  }
}`))

// config is the decoded web-crawl configuration (SPEC-04 §2). It is shared with the
// sitemap connector (SPEC-04 §3, STORY-07.5), which populates SitemapURLs instead of
// StartURLs and drives the same crawl core; a field unused by a given kind is simply
// absent from that kind's JSON Schema (additionalProperties:false rejects a stray one).
type config struct {
	StartURLs        []string `json:"start_urls"`
	SitemapURLs      []string `json:"sitemap_urls"` // STORY-07.5 (sitemap connector)
	Allow            []string `json:"allow"`
	Deny             []string `json:"deny"`
	MaxDepth         int      `json:"max_depth"`
	MaxPages         int      `json:"max_pages"`
	DelayMS          int      `json:"delay_ms"`
	Concurrency      int      `json:"concurrency"`
	IncludeSelectors []string `json:"include_selectors"` // STORY-07.3
	ExcludeSelectors []string `json:"exclude_selectors"` // STORY-07.3
	RenderJS         bool     `json:"render_js"`
}

// Defaults applied when a field is absent or non-positive.
const (
	defaultMaxDepth    = 3
	defaultMaxPages    = 1000
	defaultConcurrency = 4
)

// withDefaults returns a copy of the config with sane defaults filled in.
func (c config) withDefaults() config {
	if c.MaxDepth <= 0 {
		c.MaxDepth = defaultMaxDepth
	}
	if c.MaxPages <= 0 {
		c.MaxPages = defaultMaxPages
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultConcurrency
	}
	if c.DelayMS < 0 {
		c.DelayMS = 0
	}
	return c
}

// withSitemapDefaults returns a copy of the config prepared for a sitemap sync: the
// frontier is exactly the sitemap's URLs with NO link following, so max_depth is
// pinned to 0 (seeds live at depth 0). The remaining politeness/limit defaults match
// the web crawler.
func (c config) withSitemapDefaults() config {
	c.MaxDepth = 0 // no link following; the frontier is exactly the sitemap URLs
	if c.MaxPages <= 0 {
		c.MaxPages = defaultMaxPages
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultConcurrency
	}
	if c.DelayMS < 0 {
		c.DelayMS = 0
	}
	return c
}

// webCrawlConnector implements connector.Connector for KindWebCrawl.
type webCrawlConnector struct{}

// New returns a fresh web-crawl connector.
func New() connector.Connector { return webCrawlConnector{} }

func (webCrawlConnector) Kind() connector.Kind { return connector.KindWebCrawl }

// ValidateConfig checks the config against the JSON schema and then the semantic
// rules the schema cannot express: every start_url must be an http(s) URL, and
// render_js (a v2 feature) must not be requested.
func (webCrawlConnector) ValidateConfig(cfg json.RawMessage) error {
	if err := configSchema.Validate(cfg); err != nil {
		return err
	}
	var c config
	if err := json.Unmarshal(cfg, &c); err != nil {
		return &connector.ConfigError{Fields: []connector.FieldError{{Message: "config must be valid JSON"}}}
	}
	return validateSemantics(c)
}

func validateSemantics(c config) error {
	if c.RenderJS {
		return &connector.ConfigError{Fields: []connector.FieldError{{
			Field:   "render_js",
			Message: "render_js is not supported yet (v2 headless rendering, SPEC-04 §2)",
		}}}
	}
	for i, raw := range c.StartURLs {
		if _, _, err := parseAndNormalize(raw); err != nil {
			return &connector.ConfigError{Fields: []connector.FieldError{{
				Field:   fmt.Sprintf("start_urls.%d", i),
				Message: "must be an absolute http(s) URL",
			}}}
		}
	}
	return nil
}

// Test validates the configuration (FR-SRC-14). Live reachability/credential
// probing across all connector kinds is STORY-07.8; here Test guarantees the
// config is well-formed and fetchable-in-principle without performing network I/O,
// so an obviously invalid source is rejected before it is ever scheduled.
func (webCrawlConnector) Test(_ context.Context, cfg json.RawMessage, _ connector.Credentials) error {
	return webCrawlConnector{}.ValidateConfig(cfg)
}

// Sync enumerates the website into the sink (SPEC-04 §2). It decodes the config,
// applies defaults, builds a crawler with the default egress client, and runs it.
func (webCrawlConnector) Sync(ctx context.Context, run connector.SyncRun, sink connector.Sink) (connector.Stats, error) {
	var c config
	if err := json.Unmarshal(run.Config, &c); err != nil {
		return connector.Stats{}, fmt.Errorf("webcrawl: decode config: %w", err)
	}
	if err := validateSemantics(c); err != nil {
		return connector.Stats{}, err
	}
	cr := newCrawler(c.withDefaults(), syncDoer)
	return cr.run(ctx, run, sink)
}

func init() {
	connector.Register(connector.KindWebCrawl, New)
}
