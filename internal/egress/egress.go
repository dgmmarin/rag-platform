// Package egress is the shared outbound-network SSRF guard (SPEC-09 §4,
// NFR-SEC-04). Any worker-side fetch of an operator- or tenant-supplied URL — the
// web crawler (STORY-07.2), the sitemap connector (STORY-07.5) and the HTTP API
// connector (STORY-07.6) — must resolve DNS and refuse to connect to private,
// loopback, link-local, or cloud-metadata addresses, re-checked on every redirect
// hop.
//
// The guard lives at the IP/network layer, deliberately below the crawler's
// scheme+host+path allowlist (which gates the frontier, STORY-07.1/ADR-0043): the
// allowlist decides WHICH URLs are in scope; this package decides which resolved IP
// addresses may ever be dialed, so a rebinding or misconfigured host cannot reach
// the internal network even if it slips past the frontier. See ADR-0044.
//
// DNS-rebinding / TOCTOU safety is structural: the check is a net.Dialer.Control
// hook, which runs AFTER name resolution on the concrete IP:port about to be
// connected. A pre-resolve check followed by an ordinary dial re-resolves and is
// rebinding-vulnerable; Control validates the exact address the socket will use, so
// a hostname that resolves clean and then rebinds to a private IP is still blocked.
package egress

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrBlocked is the sentinel every SSRF refusal wraps, so callers can classify a
// blocked dial with errors.Is without importing the concrete type.
var ErrBlocked = errors.New("egress: destination address blocked by SSRF guard")

// BlockedError reports that a dial to IP was refused because IP is not a public,
// globally-routable unicast address (SPEC-09 §4).
type BlockedError struct{ IP net.IP }

func (e *BlockedError) Error() string {
	return fmt.Sprintf("egress: blocked dial to non-public address %s (SPEC-09 §4 SSRF guard)", e.IP)
}

func (e *BlockedError) Unwrap() error { return ErrBlocked }

// dialTimeout bounds the per-connection dial (distinct from the request-level
// timeout the *http.Client applies to the whole exchange).
const dialTimeout = 10 * time.Second

// IsBlocked reports whether ip is in a network class the SSRF guard forbids.
// Blocked: unspecified (0.0.0.0, ::), loopback (127/8, ::1), private (RFC1918
// 10/8·172.16/12·192.168/16 and IPv6 unique-local fc00::/7), link-local
// (169.254/16 — including the 169.254.169.254 cloud-metadata IP — and fe80::/10),
// multicast (incl. link-/interface-local), and the IPv4 broadcast address.
// IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) are unwrapped to their v4 form first,
// so a private address cannot be smuggled through the mapped encoding. Only a
// global-unicast public address is allowed; anything else fails closed.
func IsBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// Unwrap IPv4-mapped IPv6 (::ffff:a.b.c.d) so the v4 classification applies.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified(),
		ip.IsLoopback(),
		ip.IsPrivate(),          // RFC1918 + IPv6 ULA fc00::/7
		ip.IsLinkLocalUnicast(), // 169.254/16 (metadata), fe80::/10
		ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(),
		ip.IsMulticast(),
		ip.Equal(net.IPv4bcast):
		return true
	}
	// Belt-and-suspenders explicit IPv6 unique-local fc00::/7, independent of the
	// stdlib IsPrivate version. (A 16-byte non-mapped address at this point.)
	if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
		return true
	}
	// Everything that is not a globally-routable unicast address is refused
	// (documentation, benchmarking, reserved, and any future class the named
	// predicates miss).
	return !ip.IsGlobalUnicast()
}

// Control is a net.Dialer.Control hook. It runs after DNS resolution on the
// concrete resolved IP:port, so it is the TOCTOU/DNS-rebinding-safe enforcement
// point: the address it validates is exactly the one the socket will connect to.
func Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("egress: unparseable dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control only ever sees a resolved literal IP; a hostname here means the
		// dialer skipped resolution — fail closed rather than trust it.
		return fmt.Errorf("egress: dial address %q is not a resolved IP (%w)", address, ErrBlocked)
	}
	if IsBlocked(ip) {
		return &BlockedError{IP: ip}
	}
	return nil
}

// GuardedDialer returns a *net.Dialer whose Control hook enforces IsBlocked on the
// resolved address of every connection (and thus every redirect hop that re-dials).
func GuardedDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: Control}
}

// GuardedTransport clones http.DefaultTransport and installs the guarded dialer as
// its DialContext. The clone preserves the stdlib defaults (connection pooling,
// proxy support, HTTP/2) while making every dial pass the SSRF check.
func GuardedTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = GuardedDialer(dialTimeout).DialContext
	return t
}

// GuardedClient returns an *http.Client wired with the guarded transport, an
// overall request timeout, and a redirect cap. Because each redirect hop re-dials
// through the guarded DialContext, a redirect to a private address is blocked at
// connect on that hop — no separate per-hop check is needed.
func GuardedClient(timeout time.Duration, maxRedirects int) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: GuardedTransport(),
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}
