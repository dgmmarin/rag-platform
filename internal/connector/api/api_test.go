package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/connector"
)

// recSink records the documents a Sync emits.
type recSink struct {
	mu        sync.Mutex
	docs      []connector.Document
	completes int
}

func (s *recSink) Put(_ context.Context, doc connector.Document) (bool, error) {
	if doc.Body != nil {
		_, _ = io.ReadAll(doc.Body)
		_ = doc.Body.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs = append(s.docs, doc)
	return true, nil
}

func (s *recSink) Complete(_ context.Context) error {
	s.mu.Lock()
	s.completes++
	s.mu.Unlock()
	return nil
}

func (s *recSink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.docs) }

func mustConfig(t *testing.T, c apiConfig) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return b
}

// TestConnectorRegisteredAndKind confirms init() registered the API connector.
func TestConnectorRegisteredAndKind(t *testing.T) {
	c, ok := connector.Lookup(connector.KindAPI)
	if !ok {
		t.Fatalf("KindAPI not registered")
	}
	if c.Kind() != connector.KindAPI {
		t.Fatalf("Kind() = %q, want %q", c.Kind(), connector.KindAPI)
	}
}

// TestSyncAuthPaginationMatrix is the AC's "fixture server tests for each
// combination": every auth type crossed with every pagination type, driven through
// the real connector.Sync HTTP path, asserting all 7 fixture items are enumerated.
func TestSyncAuthPaginationMatrix(t *testing.T) {
	restore := syncClient
	t.Cleanup(func() { syncClient = restore })

	for _, at := range authTypes {
		for _, pt := range pagTypes {
			at, pt := at, pt
			t.Run(at+"/"+pt, func(t *testing.T) {
				srv, _, _ := newAuthFixture(t, at, pt)
				SetEgressClientForTest(srv.Client())

				cfg := apiConfig{
					BaseURL: srv.URL,
					Auth:    authConfigFor(at, srv.URL+"/token"),
					Endpoints: []endpoint{{
						Name: "items", Path: "/items", Method: http.MethodGet,
						Pagination: paginationConfig(pt), ItemsPath: "$.data", IDPath: "$.id",
					}},
				}
				run := connector.SyncRun{
					SourceID: uuid.New(),
					Config:   mustConfig(t, cfg),
					Creds:    connector.Credentials(credsFor(at)),
					Full:     true,
					Log:      apiTestLogger(),
				}
				sink := &recSink{}
				stats, err := New().Sync(context.Background(), run, sink)
				if err != nil {
					t.Fatalf("Sync(%s/%s): %v", at, pt, err)
				}
				if sink.count() != fixtureTotal {
					t.Fatalf("Sync(%s/%s) emitted %d docs, want %d", at, pt, sink.count(), fixtureTotal)
				}
				if stats.DocsSeen != fixtureTotal {
					t.Fatalf("stats.DocsSeen = %d, want %d", stats.DocsSeen, fixtureTotal)
				}
				// Placeholder mapping (07.6): ExternalID from id_path, JSON body.
				for _, d := range sink.docs {
					if !strings.HasPrefix(d.ExternalID, "items/") {
						t.Fatalf("ExternalID %q not namespaced to endpoint", d.ExternalID)
					}
					if d.MimeType != "application/json" || d.Text == "" {
						t.Fatalf("placeholder doc should carry raw JSON text; got mime=%q text=%q", d.MimeType, d.Text)
					}
				}
			})
		}
	}
}

// TestOAuth2TokenCachedThenRefreshed proves the oauth2 client-credentials flow is
// handled by golang.org/x/oauth2: a long-lived token is fetched once and reused
// across pages; a short-lived token is auto-refreshed (more than one token fetch).
func TestOAuth2TokenCachedThenRefreshed(t *testing.T) {
	restore := syncClient
	t.Cleanup(func() { syncClient = restore })

	run := func(expiry int) *authChecker {
		ph := &paginationHandler{pagType: "page"}
		ac := &authChecker{authType: "oauth2_cc", tokenExpiry: expiry, next: ph}
		srv := httptest.NewServer(ac)
		t.Cleanup(srv.Close)
		SetEgressClientForTest(srv.Client())

		cfg := apiConfig{
			BaseURL: srv.URL,
			Auth:    authConfig{Type: "oauth2_cc", TokenURL: srv.URL + "/token"},
			Endpoints: []endpoint{{
				Name: "items", Path: "/items", Method: http.MethodGet,
				Pagination: paginationConfig("page"), ItemsPath: "$.data", IDPath: "$.id",
			}},
		}
		sink := &recSink{}
		_, err := New().Sync(context.Background(), connector.SyncRun{
			SourceID: uuid.New(), Config: mustConfig(t, cfg),
			Creds: connector.Credentials(credsFor("oauth2_cc")), Full: true, Log: apiTestLogger(),
		}, sink)
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
		if sink.count() != fixtureTotal {
			t.Fatalf("emitted %d, want %d", sink.count(), fixtureTotal)
		}
		return ac
	}

	// Long-lived token: fetched once, reused across all page requests.
	if hits := run(3600).tokenHits(); hits != 1 {
		t.Fatalf("long-lived token fetched %d times, want 1 (not cached by x/oauth2)", hits)
	}
	// Short-lived token: x/oauth2 refreshes it (expiryDelta makes it stale), so the
	// token endpoint is hit more than once across the paged enumeration.
	if hits := run(1).tokenHits(); hits < 2 {
		t.Fatalf("short-lived token fetched %d times, want >=2 (not refreshed by x/oauth2)", hits)
	}
}

func TestValidateConfig(t *testing.T) {
	good := `{
      "base_url":"https://api.acme.com",
      "auth":{"type":"bearer"},
      "endpoints":[{"name":"products","path":"/v1/products","method":"GET",
        "pagination":{"type":"cursor","cursor_param":"cursor","cursor_path":"$.next_cursor"},
        "items_path":"$.data","id_path":"$.id"}]
    }`
	if err := New().ValidateConfig(json.RawMessage(good)); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	// A full 07.7-shaped config (template/uri_template/metadata/updated_path) must
	// also validate now, so the seam is forward-compatible.
	full := `{
      "base_url":"https://api.acme.com",
      "auth":{"type":"oauth2_cc","token_url":"https://api.acme.com/oauth/token","scopes":["read"]},
      "endpoints":[{"name":"p","path":"/v1/p","method":"GET",
        "pagination":{"type":"page","page_param":"page","size_param":"per_page","size":100},
        "items_path":"$.data","id_path":"$.id","updated_path":"$.updated_at",
        "incremental_param":"updated_since",
        "template":"# {{.name}}","uri_template":"https://acme.com/p/{{.slug}}",
        "metadata":{"category":"$.category.name"}}]
    }`
	if err := New().ValidateConfig(json.RawMessage(full)); err != nil {
		t.Fatalf("valid full config rejected: %v", err)
	}

	bad := []string{
		`{}`, // no base_url / endpoints
		`{"base_url":"https://x","auth":{"type":"nope"},"endpoints":[{"name":"a","path":"/a","pagination":{"type":"none"},"items_path":"$.data"}]}`,      // bad auth type
		`{"base_url":"https://x","auth":{"type":"bearer"},"endpoints":[{"name":"a","path":"/a","pagination":{"type":"weird"},"items_path":"$.data"}]}`,   // bad pagination type
		`{"base_url":"not-a-url","auth":{"type":"bearer"},"endpoints":[{"name":"a","path":"/a","pagination":{"type":"none"},"items_path":"$.data"}]}`,    // base_url not http(s)
		`{"base_url":"https://x","auth":{"type":"oauth2_cc"},"endpoints":[{"name":"a","path":"/a","pagination":{"type":"none"},"items_path":"$.data"}]}`, // oauth2 needs token_url
	}
	for i, b := range bad {
		if err := New().ValidateConfig(json.RawMessage(b)); err == nil {
			t.Fatalf("bad config #%d accepted: %s", i, b)
		}
	}
}
