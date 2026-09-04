package webcrawl

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/egress"
)

// TestDefaultDoerIsSSRFGuarded proves the connector's PRODUCTION egress default is
// fail-closed: defaultDoer() (what Sync uses when no test doer is injected) refuses
// to connect to a loopback address. If this ever regresses to the old permissive
// default, a tenant could point the crawler at the internal network (SPEC-09 §4).
func TestDefaultDoerIsSSRFGuarded(t *testing.T) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:9/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	_, err = defaultDoer().Do(req)
	if err == nil {
		t.Fatal("defaultDoer reached loopback; production egress must be SSRF-guarded")
	}
	if !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("defaultDoer error = %v, want an SSRF block (egress.ErrBlocked)", err)
	}
}

// TestSizeCapRejectsOverCapBody proves the SPEC-09 §4 "max response size 20 MB" is
// actually enforced in the crawler read path: a response body over the cap is
// rejected (not emitted, recorded as an error) so a huge body cannot exhaust memory.
// The cap is exercised through the crawler's injectable maxBytes so the test need
// not stream 20 MB.
func TestSizeCapRejectsOverCapBody(t *testing.T) {
	const cap = 1024
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>" + strings.Repeat("x", cap*4) + "</body></html>"))
	})
	mux.HandleFunc("/", page(`<a href="/big">big</a>`))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config{StartURLs: []string{srv.URL + "/"}, MaxDepth: 1, MaxPages: 100, Concurrency: 1}.withDefaults()
	sink := newRecSink()
	cr := newCrawler(cfg, srv.Client())
	cr.maxBytes = cap
	if _, err := cr.run(context.Background(), syncRun(newMemPageStore()), sink); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.ids()[srv.URL+"/big"] {
		t.Fatal("over-cap /big was emitted; the 20 MB size cap is not enforced")
	}
	// The root (under cap) is still emitted — the cap rejects only the oversize page.
	if !sink.ids()[srv.URL+"/"] {
		t.Fatal("under-cap root was not emitted; size cap over-rejected")
	}
}
