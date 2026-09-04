package webcrawl

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/rag-platform/ragctl/internal/connector"
	"strings"
	"testing"
)

// contentPage is a page with a clearly-marked article surrounded by realistic
// site chrome (header/nav, sidebar, cookie banner, footer). CONTENT_* tokens
// live in the article; BOILER_* tokens live in the chrome.
const contentPage = `<!doctype html>
<html><head>
  <title>The Article Title</title>
  <meta property="og:title" content="OG Social Title"/>
</head><body>
  <header class="site-header"><nav><a href="/">BOILER_NAV_HOME</a><a href="/about">BOILER_NAV_ABOUT</a></nav></header>
  <div class="cookie-banner">BOILER_COOKIE_NOTICE <button>Accept</button></div>
  <main class="post-body">
    <h1>The Article Heading</h1>
    <p>First paragraph with CONTENT_ONE inside it.</p>
    <p>Second paragraph mentioning CONTENT_TWO for good measure.</p>
    <ul><li>CONTENT_LIST_ITEM</li></ul>
  </main>
  <aside class="sidebar related"><p>BOILER_RELATED_LINKS</p><div class="ad">BOILER_AD_UNIT</div></aside>
  <footer class="site-footer">BOILER_FOOTER_COPYRIGHT</footer>
</body></html>`

func TestExtractContentReadabilityFallbackRemovesChrome(t *testing.T) {
	// No selectors configured -> the semantic readability fallback (ADR-0034
	// parse.htmlParser) must keep the <main> article and drop nav/cookie/aside/footer.
	md := extractContent([]byte(contentPage), nil, nil)
	if md == "" {
		t.Fatal("extractContent returned empty markdown")
	}
	for _, want := range []string{"CONTENT_ONE", "CONTENT_TWO", "CONTENT_LIST_ITEM"} {
		if !strings.Contains(md, want) {
			t.Errorf("content marker %q missing from output:\n%s", want, md)
		}
	}
	for _, boiler := range []string{"BOILER_NAV_HOME", "BOILER_COOKIE_NOTICE", "BOILER_RELATED_LINKS", "BOILER_AD_UNIT", "BOILER_FOOTER_COPYRIGHT"} {
		if strings.Contains(md, boiler) {
			t.Errorf("boilerplate marker %q leaked into output:\n%s", boiler, md)
		}
	}
}

func TestExtractContentIncludeSelectorKeepsOnlyMatch(t *testing.T) {
	// include ["main"] keeps only the <main> subtree; everything outside is dropped
	// even if the readability heuristic would otherwise have kept it.
	md := extractContent([]byte(contentPage), []string{"main"}, nil)
	if !strings.Contains(md, "CONTENT_ONE") {
		t.Errorf("article content dropped by include selector:\n%s", md)
	}
	if strings.Contains(md, "BOILER_NAV_HOME") || strings.Contains(md, "BOILER_FOOTER_COPYRIGHT") {
		t.Errorf("include selector leaked out-of-subtree chrome:\n%s", md)
	}
}

func TestExtractContentExcludeSelectorDropsSubtree(t *testing.T) {
	// exclude applies even alongside include: include the whole body but drop the
	// cookie banner and ad by class selector.
	page := `<html><body><div id="wrap">
	  <p>CONTENT_KEEP</p>
	  <div class="cookie-banner">BOILER_COOKIE</div>
	  <div class="ad">BOILER_AD</div>
	</div></body></html>`
	md := extractContent([]byte(page), []string{"#wrap"}, []string{".cookie-banner", ".ad"})
	if !strings.Contains(md, "CONTENT_KEEP") {
		t.Errorf("kept content dropped:\n%s", md)
	}
	if strings.Contains(md, "BOILER_COOKIE") || strings.Contains(md, "BOILER_AD") {
		t.Errorf("exclude selector failed to drop subtree:\n%s", md)
	}
}

// firstDoc returns the sole document a single-page crawl emitted.
func firstDoc(t *testing.T, s *recSink) connector.Document {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.byID) != 1 {
		t.Fatalf("want exactly one emitted doc, got %d", len(s.byID))
	}
	for _, d := range s.byID {
		return d
	}
	return connector.Document{}
}

func TestCrawlHTMLEmittedAsMarkdownText(t *testing.T) {
	// An HTML page must be emitted as extracted markdown in Document.Text (SPEC-04
	// §2: "HTML → markdown"), MimeType text/markdown, with no raw Body — the
	// boilerplate stripped, the article kept.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(contentPage))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 1, MaxPages: 1, Concurrency: 1}.withDefaults()
	sink := newRecSink()
	if _, err := newCrawler(cfg, srv.Client()).run(context.Background(), syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The page's canonical ExternalID is its normalised URL (no <link canonical>).
	var doc = firstDoc(t, sink)
	if doc.Body != nil {
		t.Error("HTML Document must not carry a raw Body when Text is set")
	}
	if doc.MimeType != "text/markdown" {
		t.Errorf("MimeType = %q, want text/markdown", doc.MimeType)
	}
	if !strings.Contains(doc.Text, "CONTENT_ONE") {
		t.Errorf("article content missing from Text:\n%s", doc.Text)
	}
	if strings.Contains(doc.Text, "BOILER_NAV_HOME") || strings.Contains(doc.Text, "BOILER_FOOTER_COPYRIGHT") {
		t.Errorf("boilerplate leaked into Text:\n%s", doc.Text)
	}
	if doc.Title != "The Article Title" {
		t.Errorf("Title = %q, want 'The Article Title'", doc.Title)
	}
}

func TestExtractTitlePrecedence(t *testing.T) {
	cases := []struct {
		name string
		html string
		want string
	}{
		{"title wins", `<html><head><title>T</title><meta property="og:title" content="OG"></head><body><h1>H</h1></body></html>`, "T"},
		{"og:title when no title", `<html><head><meta property="og:title" content="OG"></head><body><h1>H</h1></body></html>`, "OG"},
		{"h1 when no title/og", `<html><head></head><body><h1>H</h1><p>x</p></body></html>`, "H"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex, err := extractHTML([]byte(tc.html), mustURL(t, "https://x/"))
			if err != nil {
				t.Fatal(err)
			}
			if ex.title != tc.want {
				t.Errorf("title = %q, want %q", ex.title, tc.want)
			}
		})
	}
}
