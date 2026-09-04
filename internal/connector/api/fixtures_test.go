package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// This file holds the httptest fixture servers the auth×pagination matrix tests
// drive. Everything is in-process and hermetic — no DB, no object storage, no real
// network (SPEC-04 §7 step 4: "integration test against a recorded fixture server").

// fixtureItems is the corpus every paginated endpoint serves: 7 records across a
// page size of 3, so pagination must walk 3 pages (3+3+1) to enumerate them all.
const (
	fixtureTotal    = 7
	fixturePageSize = 3
)

func fixtureItem(i int) map[string]any {
	return map[string]any{"id": fmt.Sprintf("i%d", i), "name": fmt.Sprintf("Item %d", i)}
}

func allFixtureItems() []map[string]any {
	out := make([]map[string]any, fixtureTotal)
	for i := range out {
		out[i] = fixtureItem(i)
	}
	return out
}

// slicePage returns the items for a zero-based page number.
func slicePage(page int) []map[string]any {
	start := page * fixturePageSize
	if start >= fixtureTotal {
		return []map[string]any{}
	}
	end := start + fixturePageSize
	if end > fixtureTotal {
		end = fixtureTotal
	}
	return allFixtureItems()[start:end]
}

// paginationHandler serves fixtureItems according to pagType. It records how many
// resource requests it received (to assert pagination actually walked pages).
type paginationHandler struct {
	pagType string
	reqs    int32
}

func (h *paginationHandler) hits() int { return int(atomic.LoadInt32(&h.reqs)) }

func (h *paginationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&h.reqs, 1)
	w.Header().Set("Content-Type", "application/json")
	q := r.URL.Query()

	switch h.pagType {
	case "none":
		writeJSON(w, map[string]any{"data": allFixtureItems()})

	case "page":
		page, _ := strconv.Atoi(q.Get("page")) // fixture pages are 1-based
		writeJSON(w, map[string]any{"data": slicePage(page - 1)})

	case "offset":
		off, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = fixturePageSize
		}
		items := allFixtureItems()
		if off < 0 {
			off = 0
		}
		if off > len(items) {
			off = len(items)
		}
		end := off + limit
		if end > len(items) {
			end = len(items)
		}
		writeJSON(w, map[string]any{"data": items[off:end]})

	case "cursor":
		// cursor "" -> page 0 (next c1); "c1" -> page 1 (next c2); "c2" -> page 2 (last).
		cur := q.Get("cursor")
		page := 0
		switch cur {
		case "":
			page = 0
		case "c1":
			page = 1
		case "c2":
			page = 2
		}
		resp := map[string]any{"data": slicePage(page)}
		if next := page + 1; next*fixturePageSize < fixtureTotal {
			resp["next_cursor"] = "c" + strconv.Itoa(next)
		}
		writeJSON(w, resp)

	case "link-header":
		page, _ := strconv.Atoi(q.Get("page"))
		if page == 0 {
			page = 1
		}
		if next := page; next*fixturePageSize < fixtureTotal {
			nextURL := *r.URL
			nq := nextURL.Query()
			nq.Set("page", strconv.Itoa(page+1))
			nextURL.RawQuery = nq.Encode()
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, nextURL.RequestURI()))
		}
		writeJSON(w, map[string]any{"data": slicePage(page - 1)})

	default:
		http.Error(w, "unknown pagination", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

// paginationConfig returns the endpoint pagination config that matches the fixture
// server's pagType, wired to the fixture's param names and page size.
func paginationConfig(pagType string) pagination {
	switch pagType {
	case "none":
		return pagination{Type: "none"}
	case "page":
		return pagination{Type: "page", PageParam: "page", SizeParam: "per_page", Size: fixturePageSize, StartPage: 1}
	case "offset":
		return pagination{Type: "offset", OffsetParam: "offset", LimitParam: "limit", Size: fixturePageSize}
	case "cursor":
		return pagination{Type: "cursor", CursorParam: "cursor", CursorPath: "$.next_cursor"}
	case "link-header":
		return pagination{Type: "link-header"}
	default:
		panic("unknown pagType " + pagType)
	}
}

var pagTypes = []string{"none", "page", "offset", "cursor", "link-header"}

// --- auth fixtures ----------------------------------------------------------

// wantCreds are the credential values the auth fixtures expect. The connector reads
// these from connector.Credentials (decrypted), never from config.
const (
	wantAPIKey       = "secret-api-key"
	wantBearer       = "secret-bearer-token"
	wantBasicUser    = "alice"
	wantBasicPass    = "s3cr3t"
	wantClientID     = "client-abc"
	wantClientSecret = "client-xyz"
	apiKeyHeaderName = "X-API-Key"
)

// authChecker wraps a resource handler, enforcing authType before serving. For
// oauth2_cc it also mints tokens at /token and counts token requests so a test can
// prove the token is fetched and refreshed by golang.org/x/oauth2.
type authChecker struct {
	authType    string
	tokenExpiry int // seconds; a small value forces x/oauth2 to refresh
	tokenReqs   int32
	next        http.Handler
	issued      atomic.Value // last issued oauth token string
}

func (a *authChecker) tokenHits() int { return int(atomic.LoadInt32(&a.tokenReqs)) }

func (a *authChecker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.authType == "oauth2_cc" && r.URL.Path == "/token" {
		a.serveToken(w, r)
		return
	}
	if !a.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	a.next.ServeHTTP(w, r)
}

func (a *authChecker) serveToken(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&a.tokenReqs, 1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// Client credentials may arrive as Basic auth or form fields (x/oauth2 auto-detects).
	id, secret := r.Form.Get("client_id"), r.Form.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		id, secret = u, p
	}
	if id != wantClientID || secret != wantClientSecret {
		http.Error(w, "invalid_client", http.StatusUnauthorized)
		return
	}
	tok := fmt.Sprintf("tok-%d", a.tokenHits())
	a.issued.Store(tok)
	exp := a.tokenExpiry
	if exp == 0 {
		exp = 3600
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": exp})
}

func (a *authChecker) authorized(r *http.Request) bool {
	switch a.authType {
	case "api_key_header":
		return r.Header.Get(apiKeyHeaderName) == wantAPIKey
	case "bearer":
		return r.Header.Get("Authorization") == "Bearer "+wantBearer
	case "basic":
		u, p, ok := r.BasicAuth()
		return ok && u == wantBasicUser && p == wantBasicPass
	case "oauth2_cc":
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		iss, _ := a.issued.Load().(string)
		return got != "" && got == iss
	default:
		return false
	}
}

// authConfigFor returns the non-secret auth shape for authType (SPEC-04 §4: config
// carries which auth type / header name / token URL, never the secret value).
func authConfigFor(authType, tokenURL string) authConfig {
	switch authType {
	case "api_key_header":
		return authConfig{Type: "api_key_header", Header: apiKeyHeaderName}
	case "bearer":
		return authConfig{Type: "bearer"}
	case "basic":
		return authConfig{Type: "basic"}
	case "oauth2_cc":
		return authConfig{Type: "oauth2_cc", TokenURL: tokenURL}
	default:
		panic("unknown authType " + authType)
	}
}

// credsFor returns the decrypted Credentials map for authType (what STORY-06.2's
// decrypt path hands the connector).
func credsFor(authType string) map[string]string {
	switch authType {
	case "api_key_header":
		return map[string]string{credKeyAPIKey: wantAPIKey}
	case "bearer":
		return map[string]string{credKeyToken: wantBearer}
	case "basic":
		return map[string]string{credKeyUsername: wantBasicUser, credKeyPassword: wantBasicPass}
	case "oauth2_cc":
		return map[string]string{credKeyClientID: wantClientID, credKeyClientSecret: wantClientSecret}
	default:
		panic("unknown authType " + authType)
	}
}

var authTypes = []string{"api_key_header", "bearer", "basic", "oauth2_cc"}

// newAuthFixture builds a fixture server enforcing authType and serving pagType.
func newAuthFixture(t *testing.T, authType, pagType string) (*httptest.Server, *authChecker, *paginationHandler) {
	t.Helper()
	ph := &paginationHandler{pagType: pagType}
	ac := &authChecker{authType: authType, next: ph}
	srv := httptest.NewServer(ac)
	t.Cleanup(srv.Close)
	return srv, ac, ph
}
