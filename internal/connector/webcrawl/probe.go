package webcrawl

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"

	"github.com/rag-platform/ragctl/internal/egress"
)

// probeReachable performs ONE GET of rawURL through the crawler's egress Doer
// (SSRF-guarded in production) and maps the outcome to an actionable, secret-free
// error for "test connection" (FR-SRC-14, STORY-07.8). A 2xx/3xx is success; a
// transport failure is classified (DNS / SSRF-block / refused / timeout) by the
// shared egress classifier; a 4xx/5xx reports the status. The response body is never
// read — this is a liveness/credential check, not a fetch — and ctx carries the
// ≤10 s probe deadline the caller derives.
func probeReachable(ctx context.Context, doer Doer, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return errors.New("start URL is not a valid request target")
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := doer.Do(req)
	if err != nil {
		return errors.New(egress.ClassifyError(err, hostOf(rawURL)))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	return errors.New("start URL returned " + strings.TrimSpace(resp.Status))
}

// transportMessage returns an actionable message and true when err is a recognised
// transport-level failure (SSRF-block, DNS, timeout, refused). false means err is an
// application-level failure (e.g. an HTTP status from fetchSitemap) the caller should
// describe itself. It lets the sitemap probe reuse the crawler's fetchSitemap (which
// collapses transport and status errors into one return) while still classifying the
// transport cases through the shared egress classifier.
func transportMessage(err error, host string) (string, bool) {
	if err == nil {
		return "", false
	}
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, egress.ErrBlocked),
		errors.As(err, &dnsErr),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, syscall.ECONNREFUSED):
		return egress.ClassifyError(err, host), true
	case errors.As(err, &netErr) && netErr.Timeout():
		return egress.ClassifyError(err, host), true
	}
	return "", false
}

// hostOf extracts the host of a raw URL for an actionable, secret-free message (the
// host is non-secret config; the query string, which may hold a secret, is dropped).
func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return ""
}
