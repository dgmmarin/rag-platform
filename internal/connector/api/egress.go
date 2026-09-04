package api

import (
	"net/http"
	"time"

	"github.com/rag-platform/ragctl/internal/egress"
)

// Egress limits applied to every API fetch (SPEC-09 §4). The size cap guards the
// JSON decode path against a hostile/huge response; the timeout is on the guarded
// client; the redirect cap bounds redirect chains (each hop re-dials through the
// SSRF guard).
const (
	maxResponseBytes = 20 << 20 // 20 MB
	fetchTimeout     = 30 * time.Second
	maxRedirects     = 10
)

// defaultClient returns the connector's production egress client: the SSRF-guarded
// transport (internal/egress, STORY-07.2/ADR-0044) with the request timeout and
// redirect cap. It is fail-closed — a real binary blocks private/loopback/link-local/
// metadata ranges with no composition-root wiring, and every redirect hop is
// re-validated. For oauth2_cc this same *http.Client is threaded into the oauth2
// context (oauth2.HTTPClient), so the token-endpoint fetch is SSRF-guarded too.
func defaultClient() *http.Client { return egress.GuardedClient(fetchTimeout, maxRedirects) }

// syncClient is the base HTTP client Sync uses. It defaults to the SSRF-guarded
// client (fail-closed); httptest-based tests override it with a permissive client
// (a loopback server's *http.Client) via SetEgressClientForTest. Unit-level
// paginate/auth tests bypass it by constructing the client directly.
var syncClient = defaultClient()

// SetEgressClientForTest overrides the base HTTP client Sync uses so httptest-based
// tests can reach a loopback server past the default SSRF guard. Production never
// calls it. Not safe for concurrent use; call it from test setup only.
func SetEgressClientForTest(c *http.Client) { syncClient = c }
