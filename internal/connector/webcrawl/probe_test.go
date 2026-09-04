package webcrawl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/egress"
)

// swapDoer overrides the connector's Sync/Test egress Doer for the duration of a
// test and restores it afterwards.
func swapDoer(t *testing.T, d Doer) {
	t.Helper()
	restore := syncDoer
	t.Cleanup(func() { syncDoer = restore })
	syncDoer = d
}

// permissiveDoer is a plain client that reaches loopback (bypassing the SSRF guard)
// so httptest servers on 127.0.0.1 are reachable; used for non-SSRF probe tests.
func permissiveDoer() Doer { return &http.Client{Timeout: 5 * time.Second} }

// recordDoer captures the request context deadline and returns a canned 200, so a
// test can assert Test applied the ≤10 s probe deadline without waiting.
type recordDoer struct {
	deadline    time.Time
	hasDeadline bool
	status      int
}

func (d *recordDoer) Do(req *http.Request) (*http.Response, error) {
	d.deadline, d.hasDeadline = req.Context().Deadline()
	st := d.status
	if st == 0 {
		st = http.StatusOK
	}
	return &http.Response{StatusCode: st, Status: fmt.Sprintf("%d OK", st), Body: io.NopCloser(strings.NewReader("<html></html>")), Header: make(http.Header)}, nil
}

func webCrawlCfg(startURL string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"start_urls":[%q]}`, startURL))
}

// TestWebCrawlTestReachableSuccess: a 2xx start URL is a passing test.
func TestWebCrawlTestReachableSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	swapDoer(t, srv.Client())

	if err := New().Test(context.Background(), webCrawlCfg(srv.URL), nil); err != nil {
		t.Fatalf("Test(reachable) = %v, want nil", err)
	}
}

// TestWebCrawlTestStatusError: a 5xx start URL yields an actionable status message.
func TestWebCrawlTestStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	swapDoer(t, srv.Client())

	err := New().Test(context.Background(), webCrawlCfg(srv.URL), nil)
	if err == nil {
		t.Fatal("Test(500) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "start URL returned") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Test(500) = %q, want a 'start URL returned 500' message", err)
	}
}

// TestWebCrawlTestSSRFBlock: through the REAL egress guard, a loopback start URL is
// refused with an actionable, secret-free message (NFR-SEC-04).
func TestWebCrawlTestSSRFBlock(t *testing.T) {
	swapDoer(t, egress.GuardedClient(fetchTimeout, maxRedirects))

	err := New().Test(context.Background(), webCrawlCfg("http://127.0.0.1:9/"), nil)
	if err == nil {
		t.Fatal("Test(loopback) = nil, want an SSRF-block error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not permitted") {
		t.Fatalf("Test(loopback) = %q, want an 'address not permitted' message", err)
	}
}

// TestWebCrawlTestDNSFailure: an unresolvable host is reported as host-not-found.
func TestWebCrawlTestDNSFailure(t *testing.T) {
	swapDoer(t, permissiveDoer())

	err := New().Test(context.Background(), webCrawlCfg("http://nonexistent-host.invalid/"), nil)
	if err == nil {
		t.Fatal("Test(bad DNS) = nil, want a host-not-found error")
	}
	if !strings.Contains(err.Error(), "host not found") {
		t.Fatalf("Test(bad DNS) = %q, want a 'host not found' message", err)
	}
}

// TestWebCrawlTestConnectionRefused: nothing listening → a 'refused' message.
func TestWebCrawlTestConnectionRefused(t *testing.T) {
	swapDoer(t, permissiveDoer())

	err := New().Test(context.Background(), webCrawlCfg("http://127.0.0.1:1/"), nil)
	if err == nil {
		t.Fatal("Test(closed port) = nil, want a connection error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refused") {
		t.Fatalf("Test(closed port) = %q, want a 'connection refused' message", err)
	}
}

// TestWebCrawlTestAppliesDeadline: Test derives a context deadline ≤ 10 s so a
// hanging host cannot wedge the /test request (FR-SRC-14).
func TestWebCrawlTestAppliesDeadline(t *testing.T) {
	rd := &recordDoer{}
	swapDoer(t, rd)

	before := time.Now()
	if err := New().Test(context.Background(), webCrawlCfg("http://example.com/"), nil); err != nil {
		t.Fatalf("Test = %v, want nil", err)
	}
	if !rd.hasDeadline {
		t.Fatal("Test did not apply a context deadline to the probe request")
	}
	if d := time.Until(rd.deadline); d <= 0 || d > egress.ProbeTimeout+time.Second {
		t.Fatalf("probe deadline %v out of range (want 0 < d <= %v)", d, egress.ProbeTimeout)
	}
	_ = before
}

// TestWebCrawlTestRejectsInvalidConfigBeforeNetwork: a malformed config fails fast.
func TestWebCrawlTestRejectsInvalidConfigBeforeNetwork(t *testing.T) {
	swapDoer(t, &recordDoer{})
	if err := New().Test(context.Background(), json.RawMessage(`{"start_urls":[]}`), connector.Credentials(nil)); err == nil {
		t.Fatal("Test(invalid config) = nil, want a validation error")
	}
}
