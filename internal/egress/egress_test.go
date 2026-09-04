package egress_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/egress"
)

// TestIsBlockedPerClass is the SPEC-09 §4 / NFR-SEC-04 truth table: one row per
// network class the SSRF guard must reject, and the public addresses it must let
// through. The AC demands "tests for each class" — this is that table.
func TestIsBlockedPerClass(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		blocked bool
	}{
		// --- IPv4 must-block classes ---
		{"ipv4 loopback 127.0.0.1", "127.0.0.1", true},
		{"ipv4 loopback 127.255.255.255", "127.255.255.255", true},
		{"rfc1918 10/8", "10.0.0.1", true},
		{"rfc1918 172.16/12", "172.16.5.4", true},
		{"rfc1918 172.31 edge", "172.31.255.255", true},
		{"rfc1918 192.168/16", "192.168.1.1", true},
		{"link-local 169.254/16", "169.254.1.1", true},
		{"cloud metadata 169.254.169.254", "169.254.169.254", true}, // the classic SSRF target
		{"unspecified 0.0.0.0", "0.0.0.0", true},
		{"broadcast 255.255.255.255", "255.255.255.255", true},
		{"multicast 224.0.0.1", "224.0.0.1", true},
		{"multicast 239.255.255.250", "239.255.255.250", true},

		// --- IPv6 must-block classes ---
		{"ipv6 loopback ::1", "::1", true},
		{"ipv6 unspecified ::", "::", true},
		{"ipv6 link-local fe80::/10", "fe80::1", true},
		{"ipv6 unique-local fc00::/7 (fc)", "fc00::1", true},
		{"ipv6 unique-local fc00::/7 (fd)", "fd12:3456:789a::1", true},
		{"ipv6 multicast ff00::/8", "ff02::1", true},

		// --- IPv4-mapped / IPv4-in-IPv6 forms must be unwrapped and blocked ---
		{"ipv4-mapped private ::ffff:10.0.0.1", "::ffff:10.0.0.1", true},
		{"ipv4-mapped loopback ::ffff:127.0.0.1", "::ffff:127.0.0.1", true},
		{"ipv4-mapped metadata ::ffff:169.254.169.254", "::ffff:169.254.169.254", true},

		// --- Public global-unicast addresses must be allowed ---
		{"public ipv4 1.1.1.1", "1.1.1.1", false},
		{"public ipv4 8.8.8.8", "8.8.8.8", false},
		{"public ipv6 2606:4700:4700::1111", "2606:4700:4700::1111", false},
		{"ipv4-mapped public ::ffff:8.8.8.8", "::ffff:8.8.8.8", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("test bug: %q is not a parseable IP", tc.ip)
			}
			if got := egress.IsBlocked(ip); got != tc.blocked {
				t.Fatalf("IsBlocked(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

// TestIsBlockedNilIP: a nil/unparseable IP fails closed (blocked).
func TestIsBlockedNilIP(t *testing.T) {
	if !egress.IsBlocked(nil) {
		t.Fatal("IsBlocked(nil) = false, want true (fail closed)")
	}
}

// TestControlBlocksResolvedPrivateIP proves the DNS-rebinding defence. The Control
// hook runs AFTER resolution on the concrete IP:port about to be dialed, so whatever
// a hostname resolved to — including a value that rebinds to a private range between
// a pre-check and the dial — is the value that is validated. Dialing a literal
// private IP is a faithful proxy: after resolution every dial address IS a literal
// IP, and Control rejects it before the socket connects.
func TestControlBlocksResolvedPrivateIP(t *testing.T) {
	for _, addr := range []string{
		"10.0.0.1:443",
		"127.0.0.1:8080",
		"169.254.169.254:80",
		"[::1]:80",
		"[fd00::1]:443",
		"[::ffff:10.0.0.1]:80",
	} {
		err := egress.Control("tcp", addr, nil)
		var be *egress.BlockedError
		if !errors.As(err, &be) {
			t.Fatalf("Control(%q) = %v, want *BlockedError", addr, err)
		}
	}
}

// TestControlAllowsPublicIP: a resolved public address passes Control.
func TestControlAllowsPublicIP(t *testing.T) {
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:80", "[2606:4700:4700::1111]:443"} {
		if err := egress.Control("tcp", addr, nil); err != nil {
			t.Fatalf("Control(%q) = %v, want nil", addr, err)
		}
	}
}

// TestControlRejectsUnparseableAddress: a malformed dial address fails closed.
func TestControlRejectsUnparseableAddress(t *testing.T) {
	if err := egress.Control("tcp", "not-an-ip-port", nil); err == nil {
		t.Fatal("Control(malformed) = nil, want error")
	}
	if err := egress.Control("tcp", "example.com:80", nil); err == nil {
		t.Fatal("Control(hostname:port) = nil, want error (Control only sees resolved IPs)")
	}
}

// TestGuardedDialerBlocksAtConnect: the dialer built for production refuses a
// private target at connect time (the Control hook fires inside DialContext),
// end-to-end proof that the wired dialer — not just the predicate — blocks.
func TestGuardedDialerBlocksAtConnect(t *testing.T) {
	d := egress.GuardedDialer(2 * time.Second)
	_, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:9")
	var be *egress.BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("GuardedDialer dial to loopback = %v, want *BlockedError", err)
	}
}

// TestRedirectToPrivateBlockedAtConnect proves redirect re-validation (SPEC-09 §4
// "re-validate on each redirect hop"). An entry server — standing in for a public
// host, permitted via a test-scoped Control allowance for its exact loopback
// address — 302-redirects to the cloud-metadata IP. The redirect hop re-dials
// through the SAME guard, so the SHIPPED egress.Control blocks 169.254.169.254 at
// connect; the metadata endpoint is never reached.
func TestRedirectToPrivateBlockedAtConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	entry := strings.TrimPrefix(srv.URL, "http://") // 127.0.0.1:PORT

	// A dialer whose Control permits ONLY the entry server (the "public" host) and
	// defers every other hop — the metadata redirect target — to the real guard.
	d := &net.Dialer{Timeout: 2 * time.Second, Control: func(network, address string, c syscall.RawConn) error {
		if address == entry {
			return nil
		}
		return egress.Control(network, address, c)
	}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = d.DialContext
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	_, err := client.Get(srv.URL) //nolint:noctx // failure is the assertion
	if err == nil {
		t.Fatal("redirect to 169.254.169.254 was followed; the metadata hop must be blocked")
	}
	if !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("redirect-hop error = %v, want SSRF block on the metadata address", err)
	}
}

// TestGuardedClientHasGuardedTransport: the production client is wired with the
// guarded transport (its DialContext blocks), so a request to a private host fails
// before any bytes are sent.
func TestGuardedClientHasGuardedTransport(t *testing.T) {
	c := egress.GuardedClient(5*time.Second, 10)
	_, err := c.Get("http://127.0.0.1:9/") //nolint:noctx // failure is the assertion
	if err == nil {
		t.Fatal("GuardedClient.Get(loopback) = nil error, want blocked at connect")
	}
	if !errors.Is(err, egress.ErrBlocked) {
		var be *egress.BlockedError
		if !errors.As(err, &be) {
			t.Fatalf("GuardedClient.Get(loopback) err = %v, want SSRF block", err)
		}
	}
}
