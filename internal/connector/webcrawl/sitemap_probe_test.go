package webcrawl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/egress"
)

func sitemapCfg(url string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"sitemap_urls":[%q]}`, url))
}

const validSitemapXML = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://acme.com/a</loc></url>
  <url><loc>https://acme.com/b</loc></url>
</urlset>`

// TestSitemapTestReachableSuccess: a fetchable, well-formed, non-empty sitemap passes.
func TestSitemapTestReachableSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(validSitemapXML))
	}))
	t.Cleanup(srv.Close)
	swapDoer(t, srv.Client())

	if err := NewSitemap().Test(context.Background(), sitemapCfg(srv.URL+"/sitemap.xml"), nil); err != nil {
		t.Fatalf("Test(valid sitemap) = %v, want nil", err)
	}
}

// TestSitemapTestNotXML: a non-XML response is an actionable "not valid XML" error.
func TestSitemapTestNotXML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is definitely not xml { json? }"))
	}))
	t.Cleanup(srv.Close)
	swapDoer(t, srv.Client())

	err := NewSitemap().Test(context.Background(), sitemapCfg(srv.URL+"/sitemap.xml"), nil)
	if err == nil {
		t.Fatal("Test(not xml) = nil, want an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not valid xml") {
		t.Fatalf("Test(not xml) = %q, want a 'not valid XML' message", err)
	}
}

// TestSitemapTestEmpty: a well-formed but empty sitemap is reported as empty.
func TestSitemapTestEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"></urlset>`))
	}))
	t.Cleanup(srv.Close)
	swapDoer(t, srv.Client())

	err := NewSitemap().Test(context.Background(), sitemapCfg(srv.URL+"/sitemap.xml"), nil)
	if err == nil {
		t.Fatal("Test(empty sitemap) = nil, want an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "no <url>") && !strings.Contains(strings.ToLower(err.Error()), "empty") {
		t.Fatalf("Test(empty sitemap) = %q, want an 'empty' message", err)
	}
}

// TestSitemapTestSSRFBlock: a loopback sitemap URL is refused by the real guard.
func TestSitemapTestSSRFBlock(t *testing.T) {
	swapDoer(t, egress.GuardedClient(fetchTimeout, maxRedirects))

	err := NewSitemap().Test(context.Background(), sitemapCfg("http://127.0.0.1:9/sitemap.xml"), nil)
	if err == nil {
		t.Fatal("Test(loopback sitemap) = nil, want an SSRF-block error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not permitted") {
		t.Fatalf("Test(loopback sitemap) = %q, want an 'address not permitted' message", err)
	}
}

// TestSitemapTestAppliesDeadline: the sitemap probe applies the ≤10 s budget too.
func TestSitemapTestAppliesDeadline(t *testing.T) {
	rd := &recordDoer{}
	swapDoer(t, rd)

	// The recordDoer returns non-XML, so Test will error after the fetch — but the
	// fetch request must still carry the deadline, which is all this asserts.
	_ = NewSitemap().Test(context.Background(), sitemapCfg("http://example.com/sitemap.xml"), nil)
	if !rd.hasDeadline {
		t.Fatal("sitemap Test did not apply a context deadline to the probe request")
	}
	if d := time.Until(rd.deadline); d <= 0 || d > egress.ProbeTimeout+time.Second {
		t.Fatalf("probe deadline out of range: %v", d)
	}
}
