package egress

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"
)

// ProbeTimeout is the hard deadline every connector "test connection" applies to its
// reachability/credential probe (FR-SRC-14, SPEC-04 §1: "within 10 s"), so a hanging
// host can never wedge the POST /v1/sources/{id}/test request. Each connector's Test
// derives a context.WithTimeout(ctx, ProbeTimeout) before touching the network.
const ProbeTimeout = 10 * time.Second

// ClassifyError maps a transport-level error from an *http.Client.Do (or a guarded
// dial) into a short, actionable, secret-free message a tenant admin can act on
// (FR-SRC-14). It is the shared classifier used by the web_crawl, sitemap and api
// connectors' Test so the four connectors do not duplicate the mapping (STORY-07.8).
//
// Sanitisation (C-4, SPEC-04 §6): it NEVER echoes err.Error(). A failed fetch is
// almost always a *url.Error whose string form includes the full request URL — and a
// URL can carry a secret in its query string (a cursor, an api_key). The message
// mentions only host, which is non-secret configuration the admin supplied. Returns
// "" when err is nil.
func ClassifyError(err error, host string) string {
	if err == nil {
		return ""
	}
	// SSRF guard refusal (private/loopback/link-local/cloud-metadata address).
	// Checked first: a blocked dial is the most security-relevant outcome, and the
	// admin's fix (use a public address) is specific.
	if errors.Is(err, ErrBlocked) {
		return "address not permitted (private, loopback, link-local or cloud-metadata address blocked by the SSRF guard)"
	}
	// DNS resolution failure — the host does not exist or cannot be resolved.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		name := host
		if name == "" {
			name = dnsErr.Name
		}
		return "host not found: " + name
	}
	// Deadline elapsed — the ≤10 s probe budget (or any context cancellation).
	if errors.Is(err, context.DeadlineExceeded) {
		return "connection timed out" + hostSuffix(host)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "connection timed out" + hostSuffix(host)
	}
	// Connection actively refused — nothing is listening on that host/port.
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused" + hostSuffix(host)
	}
	// Fallback: a connect/transport failure we do not classify further. We
	// deliberately keep it generic and never include err.Error() (see the
	// sanitisation note above).
	return "could not connect" + hostSuffix(host)
}

// hostSuffix appends " to <host>" when a non-empty host is known, so a message reads
// "connection refused to acme.com" rather than a bare "connection refused".
func hostSuffix(host string) string {
	if host == "" {
		return ""
	}
	return " to " + host
}
