package webcrawl

import (
	"net/url"
	"testing"
)

const samplePage = `<!doctype html>
<html><head>
  <title>  Widgets — Acme  </title>
  <link rel="canonical" href="https://acme.com/products/widget"/>
</head><body>
  <a href="/products/gadget">Gadget</a>
  <a href="https://acme.com/products/widget?utm_source=nav#reviews">Widget</a>
  <a href="mailto:sales@acme.com">Mail</a>
  <a href="https://other.example/x">External</a>
  <p>Body text.</p>
</body></html>`

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestExtractHTMLTitleAndCanonical(t *testing.T) {
	base := mustURL(t, "https://acme.com/products/widget?ref=home")
	ex, err := extractHTML([]byte(samplePage), base)
	if err != nil {
		t.Fatalf("extractHTML: %v", err)
	}
	if ex.title != "Widgets — Acme" {
		t.Fatalf("title = %q, want trimmed 'Widgets — Acme'", ex.title)
	}
	if ex.canonical != "https://acme.com/products/widget" {
		t.Fatalf("canonical = %q", ex.canonical)
	}
}

func TestExtractHTMLLinksResolvedNormalizedFiltered(t *testing.T) {
	base := mustURL(t, "https://acme.com/products/widget")
	ex, err := extractHTML([]byte(samplePage), base)
	if err != nil {
		t.Fatalf("extractHTML: %v", err)
	}
	got := map[string]bool{}
	for _, l := range ex.links {
		got[l] = true
	}
	// Relative link resolved against base; utm/fragment stripped on the widget link;
	// mailto dropped (non-http); external kept (allow/deny gating happens later).
	want := []string{
		"https://acme.com/products/gadget",
		"https://acme.com/products/widget",
		"https://other.example/x",
	}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("missing expected link %q in %v", w, ex.links)
		}
	}
	for l := range got {
		if l == "mailto:sales@acme.com" {
			t.Fatal("mailto link should have been dropped")
		}
	}
}
