package webcrawl

import (
	"net/http"
	"time"
)

// Doer is the HTTP egress seam the crawler fetches through. It is the ENTIRE
// surface the crawler uses to reach the network, so hardening egress needs no
// change to crawl logic.
//
// EGRESS/SSRF SEAM (STORY-07.2): the SSRF guard — resolve DNS and reject
// private/loopback/link-local/metadata ranges, re-validated on every redirect hop
// (SPEC-09 §4, NFR-SEC-04) — is a later story. It will be installed by replacing
// this Doer (or the *http.Transport it wraps) with a guarded one; the crawler is
// structured so that swap touches nothing here.
//
// ponytail: defaultDoer permits loopback so the httptest-based unit tests can
// reach 127.0.0.1. The 07.2 guard MUST block loopback in production; the upgrade
// path is a build/config-selected guarded transport injected at the composition
// root, with tests overriding it with a permissive one.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Egress limits applied to every fetch (SPEC-09 §4: "max response size 20 MB;
// timeouts 30 s"). The size cap is enforced in the crawler's read path; the
// timeout is on the client here.
const (
	maxResponseBytes = 20 << 20 // 20 MB
	fetchTimeout     = 30 * time.Second
	maxRedirects     = 10
)

// defaultDoer returns the crawler's default HTTP client: bounded timeout and a
// redirect cap. STORY-07.2 replaces the Transport's DialContext with an SSRF-
// guarded dialer (each redirect hop re-dials, so the guard re-validates per hop
// automatically).
func defaultDoer() Doer {
	return &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
		// Transport left as http.DefaultTransport; 07.2 swaps in the guarded one.
	}
}
