package api

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

// swapClient overrides the connector's base egress client for a test and restores it.
func swapClient(t *testing.T, c *http.Client) {
	t.Helper()
	restore := syncClient
	t.Cleanup(func() { syncClient = restore })
	syncClient = c
}

// recordRT captures the request context deadline, so a test can assert Test applies
// the ≤10 s probe deadline without waiting.
type recordRT struct {
	deadline    time.Time
	hasDeadline bool
}

func (rt *recordRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.deadline, rt.hasDeadline = req.Context().Deadline()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// testCfg builds a schema-valid config as raw JSON (avoiding the struct's nil
// scopes/metadata, which marshal to null and the JSON Schema rejects). A single
// "none"-paginated endpoint is enough for a one-request probe.
func testCfg(baseURL, tokenURL, authType string) json.RawMessage {
	var auth string
	switch authType {
	case "oauth2_cc":
		auth = fmt.Sprintf(`{"type":"oauth2_cc","token_url":%q}`, tokenURL)
	case "api_key_header":
		auth = fmt.Sprintf(`{"type":"api_key_header","header":%q}`, apiKeyHeaderName)
	default:
		auth = fmt.Sprintf(`{"type":%q}`, authType)
	}
	return json.RawMessage(fmt.Sprintf(
		`{"base_url":%q,"auth":%s,"endpoints":[{"name":"items","path":"/items","method":"GET","pagination":{"type":"none"},"items_path":"$.data","id_path":"$.id"}]}`,
		baseURL, auth))
}

// TestAPITestReachableSuccess: a reachable endpoint that accepts the credentials passes.
func TestAPITestReachableSuccess(t *testing.T) {
	srv, _, _ := newAuthFixture(t, "bearer", "none")
	swapClient(t, srv.Client())

	cfg := testCfg(srv.URL, "", "bearer")
	if err := New().Test(context.Background(), cfg, connector.Credentials(credsFor("bearer"))); err != nil {
		t.Fatalf("Test(reachable) = %v, want nil", err)
	}
}

// TestAPITestAuthFailure: wrong credentials → 401 → an actionable credential error.
func TestAPITestAuthFailure(t *testing.T) {
	srv, _, _ := newAuthFixture(t, "bearer", "none")
	swapClient(t, srv.Client())

	cfg := testCfg(srv.URL, "", "bearer")
	err := New().Test(context.Background(), cfg, connector.Credentials{credKeyToken: "wrong-token"})
	if err == nil {
		t.Fatal("Test(bad token) = nil, want a credential error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "authentication failed") {
		t.Fatalf("Test(bad token) = %q, want an 'authentication failed' message", err)
	}
}

// TestAPITestOAuthBadCredentials: the oauth2 client-credentials token fetch is
// rejected (401) → an actionable credential error (not a generic connect error).
func TestAPITestOAuthBadCredentials(t *testing.T) {
	srv, _, _ := newAuthFixture(t, "oauth2_cc", "none")
	swapClient(t, srv.Client())

	cfg := testCfg(srv.URL, srv.URL+"/token", "oauth2_cc")
	err := New().Test(context.Background(), cfg,
		connector.Credentials{credKeyClientID: wantClientID, credKeyClientSecret: "wrong-secret"})
	if err == nil {
		t.Fatal("Test(bad oauth secret) = nil, want a credential error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "authentication failed") {
		t.Fatalf("Test(bad oauth secret) = %q, want an 'authentication failed' message", err)
	}
}

// TestAPITestMissingCredential: a missing required secret fails closed, naming only
// the missing KEY (never a value).
func TestAPITestMissingCredential(t *testing.T) {
	swapClient(t, &http.Client{Transport: &recordRT{}})
	cfg := testCfg("https://api.acme.com", "", "bearer")
	err := New().Test(context.Background(), cfg, connector.Credentials(nil))
	if err == nil {
		t.Fatal("Test(missing token) = nil, want a credential error")
	}
	if !strings.Contains(err.Error(), credKeyToken) {
		t.Fatalf("Test(missing token) = %q, want a message naming the missing key %q", err, credKeyToken)
	}
}

// TestAPITestSSRFBlock: through the REAL guard, a loopback base_url is refused with an
// actionable, secret-free message.
func TestAPITestSSRFBlock(t *testing.T) {
	srv, _, _ := newAuthFixture(t, "bearer", "none") // loopback (127.0.0.1) server
	swapClient(t, egress.GuardedClient(fetchTimeout, maxRedirects))

	cfg := testCfg(srv.URL, "", "bearer")
	err := New().Test(context.Background(), cfg, connector.Credentials(credsFor("bearer")))
	if err == nil {
		t.Fatal("Test(loopback base_url) = nil, want an SSRF-block error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not permitted") {
		t.Fatalf("Test(loopback base_url) = %q, want an 'address not permitted' message", err)
	}
}

// TestAPITestStatusError: a non-2xx, non-auth status is actionable and secret-free.
func TestAPITestStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	swapClient(t, srv.Client())

	cfg := testCfg(srv.URL, "", "bearer")
	err := New().Test(context.Background(), cfg, connector.Credentials(credsFor("bearer")))
	if err == nil {
		t.Fatal("Test(500) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("Test(500) = %q, want a message reporting the status", err)
	}
}

// TestAPITestAppliesDeadline: Test derives a context deadline ≤ 10 s (FR-SRC-14).
func TestAPITestAppliesDeadline(t *testing.T) {
	rt := &recordRT{}
	swapClient(t, &http.Client{Transport: rt})

	cfg := testCfg("https://api.acme.com", "", "bearer")
	if err := New().Test(context.Background(), cfg, connector.Credentials(credsFor("bearer"))); err != nil {
		t.Fatalf("Test = %v, want nil", err)
	}
	if !rt.hasDeadline {
		t.Fatal("Test did not apply a context deadline to the probe request")
	}
	if d := time.Until(rt.deadline); d <= 0 || d > egress.ProbeTimeout+time.Second {
		t.Fatalf("probe deadline %v out of range (want 0 < d <= %v)", d, egress.ProbeTimeout)
	}
}
