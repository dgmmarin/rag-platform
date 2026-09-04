package webcrawl

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rag-platform/ragctl/internal/connector"
)

func TestKind(t *testing.T) {
	if k := New().Kind(); k != connector.KindWebCrawl {
		t.Fatalf("Kind() = %q, want %q", k, connector.KindWebCrawl)
	}
}

func TestRegisteredInDefaultRegistry(t *testing.T) {
	c, ok := connector.Lookup(connector.KindWebCrawl)
	if !ok {
		t.Fatal("web_crawl connector not registered in the default registry")
	}
	if c.Kind() != connector.KindWebCrawl {
		t.Fatalf("registered connector Kind() = %q", c.Kind())
	}
}

func TestValidateConfigAcceptsWellFormed(t *testing.T) {
	cfg := `{"start_urls":["https://docs.acme.com/"],"allow":["https://docs.acme.com/"],"deny":["/search"],"max_depth":5,"max_pages":5000,"delay_ms":500,"concurrency":8}`
	if err := New().ValidateConfig(json.RawMessage(cfg)); err != nil {
		t.Fatalf("ValidateConfig(valid) = %v, want nil", err)
	}
}

func TestValidateConfigRejects(t *testing.T) {
	cases := map[string]string{
		"missing start_urls": `{"max_depth":2}`,
		"empty start_urls":   `{"start_urls":[]}`,
		"unknown field":      `{"start_urls":["https://x/"],"nope":1}`,
		"bad type":           `{"start_urls":"https://x/"}`,
		"malformed":          `not json`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := New().ValidateConfig(json.RawMessage(cfg)); err == nil {
				t.Fatalf("ValidateConfig(%s) = nil, want error", cfg)
			}
		})
	}
}

func TestValidateConfigRejectsNonHTTPStartURL(t *testing.T) {
	if err := New().ValidateConfig(json.RawMessage(`{"start_urls":["ftp://x/"]}`)); err == nil {
		t.Fatal("ValidateConfig with ftp start url = nil, want error")
	}
}

func TestValidateConfigRejectsRenderJS(t *testing.T) {
	// render_js is a v2 feature (SPEC-04 §2); accepting it silently would be a lie.
	if err := New().ValidateConfig(json.RawMessage(`{"start_urls":["https://x/"],"render_js":true}`)); err == nil {
		t.Fatal("ValidateConfig with render_js=true = nil, want unsupported error")
	}
}

func TestTestValidatesConfigWithoutNetwork(t *testing.T) {
	// Test on a valid config with a reachable-by-nothing host still must not panic;
	// a malformed config must fail fast before any network work.
	if err := New().Test(context.Background(), json.RawMessage(`{"start_urls":[]}`), nil); err == nil {
		t.Fatal("Test(invalid config) = nil, want error")
	}
}
