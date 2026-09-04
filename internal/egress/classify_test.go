package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestClassifyErrorNil: a nil error produces no message.
func TestClassifyErrorNil(t *testing.T) {
	if got := ClassifyError(nil, "acme.com"); got != "" {
		t.Fatalf("ClassifyError(nil) = %q, want empty", got)
	}
}

// TestClassifyErrorSSRFBlock: a wrapped egress.ErrBlocked is reported as a
// permission/SSRF refusal, whatever wrapping the http stack added.
func TestClassifyErrorSSRFBlock(t *testing.T) {
	err := fmt.Errorf(`Get "http://169.254.169.254/": %w`, &BlockedError{IP: net.ParseIP("169.254.169.254")})
	got := ClassifyError(err, "169.254.169.254")
	if !strings.Contains(strings.ToLower(got), "not permitted") {
		t.Fatalf("ClassifyError(blocked) = %q, want an 'address not permitted' message", got)
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatal("precondition: wrapped error must satisfy errors.Is(ErrBlocked)")
	}
}

// TestClassifyErrorDNS: a not-found DNS error names the host actionably.
func TestClassifyErrorDNS(t *testing.T) {
	err := fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true})
	got := ClassifyError(err, "nope.invalid")
	if !strings.Contains(got, "host not found") || !strings.Contains(got, "nope.invalid") {
		t.Fatalf("ClassifyError(dns) = %q, want 'host not found: nope.invalid'", got)
	}
}

// TestClassifyErrorTimeout: the probe deadline (context) maps to a timeout message.
func TestClassifyErrorTimeout(t *testing.T) {
	got := ClassifyError(fmt.Errorf("wrap: %w", context.DeadlineExceeded), "acme.com")
	if !strings.Contains(strings.ToLower(got), "timed out") {
		t.Fatalf("ClassifyError(deadline) = %q, want a timeout message", got)
	}
}

// TestClassifyErrorNetTimeout: a net.Error whose Timeout() is true also maps to timeout.
func TestClassifyErrorNetTimeout(t *testing.T) {
	got := ClassifyError(fmt.Errorf("wrap: %w", timeoutErr{}), "acme.com")
	if !strings.Contains(strings.ToLower(got), "timed out") {
		t.Fatalf("ClassifyError(net timeout) = %q, want a timeout message", got)
	}
}

// TestClassifyErrorRefused: a refused connection is reported clearly.
func TestClassifyErrorRefused(t *testing.T) {
	got := ClassifyError(fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED), "acme.com")
	if !strings.Contains(strings.ToLower(got), "refused") {
		t.Fatalf("ClassifyError(refused) = %q, want a 'connection refused' message", got)
	}
}

// TestClassifyErrorGenericSanitised: an unclassified transport error yields a
// generic message that NEVER echoes the raw error (which, for a *url.Error, carries
// the full URL and any secret in its query string — C-4).
func TestClassifyErrorGenericSanitised(t *testing.T) {
	secret := "s3cr3t-token-value"
	raw := fmt.Errorf(`Get "https://acme.com/x?api_key=%s": connection reset by peer`, secret)
	got := ClassifyError(raw, "acme.com")
	if strings.Contains(got, secret) {
		t.Fatalf("ClassifyError leaked the secret: %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "could not connect") {
		t.Fatalf("ClassifyError(generic) = %q, want a 'could not connect' message", got)
	}
}

// TestProbeTimeoutIsTenSeconds pins the FR-SRC-14 "within 10 s" budget.
func TestProbeTimeoutIsTenSeconds(t *testing.T) {
	if ProbeTimeout != 10*time.Second {
		t.Fatalf("ProbeTimeout = %v, want 10s (FR-SRC-14)", ProbeTimeout)
	}
}

// timeoutErr is a minimal net.Error that reports Timeout() == true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }
