package webcrawl

import (
	"net/http"
	"time"

	"github.com/rag-platform/ragctl/internal/egress"
)

// Doer is the HTTP egress seam the crawler fetches through. It is the ENTIRE
// surface the crawler uses to reach the network, so hardening egress needs no
// change to crawl logic.
//
// EGRESS/SSRF SEAM (STORY-07.2, SPEC-09 §4, NFR-SEC-04): the SSRF guard lives in
// internal/egress. defaultDoer wires egress.GuardedClient, whose net.Dialer.Control
// hook rejects private/loopback/link-local/metadata ranges AFTER DNS resolution on
// the concrete dialed IP — TOCTOU/DNS-rebinding safe — and re-validates every
// redirect hop (each hop re-dials through the same guarded transport). The guard is
// the connector's fail-closed default: production Sync blocks private ranges with no
// further wiring. See ADR-0044.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Egress limits applied to every fetch (SPEC-09 §4: "max response size 20 MB;
// timeouts 30 s"). The timeout is on the guarded client; the size cap is enforced
// in the crawler's read path (crawl.go: an over-cap body is rejected, not parsed,
// so a huge response cannot exhaust memory).
const (
	maxResponseBytes = 20 << 20 // 20 MB
	fetchTimeout     = 30 * time.Second
	maxRedirects     = 10
)

// defaultDoer returns the crawler's production egress client: the SSRF-guarded
// transport (internal/egress) with the 30 s request timeout and the redirect cap.
// It is fail-closed — a real binary blocks private ranges without any composition-
// root wiring. httptest-based e2e tests, which must reach a 127.0.0.1 server,
// install a permissive Doer via SetEgressDoerForTest.
func defaultDoer() Doer {
	return egress.GuardedClient(fetchTimeout, maxRedirects)
}

// syncDoer is the egress client the connector's Sync uses. It defaults to the
// SSRF-guarded client (fail-closed, SPEC-09 §4); tests override it with a permissive
// Doer via SetEgressDoerForTest. Unit crawl tests bypass it entirely by building the
// crawler with newCrawler(cfg, doer) directly.
var syncDoer = defaultDoer()

// SetEgressDoerForTest overrides the egress client Sync uses so httptest-based e2e
// tests can reach a loopback server past the default SSRF guard. Production never
// calls it. Not safe for concurrent use; call it once from test setup.
func SetEgressDoerForTest(d Doer) { syncDoer = d }
