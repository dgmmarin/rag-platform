package api

import (
	"errors"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"

	"github.com/rag-platform/ragctl/internal/egress"
)

// classifyAPIError maps a transport-level error from the authed request (or the
// oauth2 token fetch it triggers) into an actionable, secret-free message for "test
// connection" (FR-SRC-14, STORY-07.8).
//
// An oauth2 client-credentials token fetch that the token endpoint REJECTS surfaces
// as an *oauth2.RetrieveError carrying the token-endpoint response; a 401/403 there
// is a credential problem, mapped to the same "authentication failed" message as a
// rejected API request. Its Error() carries the token-endpoint response body, so we
// deliberately do NOT echo it. Every other failure (SSRF-block, DNS, timeout,
// refused, generic — including a token fetch that could not connect) is classified by
// the shared egress classifier, which never echoes the URL/query (C-4).
func classifyAPIError(err error, host string) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.Response != nil && (re.Response.StatusCode == http.StatusUnauthorized || re.Response.StatusCode == http.StatusForbidden) {
			return "authentication failed: check credentials (the token endpoint rejected the client credentials)"
		}
		return "the token endpoint rejected the request"
	}
	return egress.ClassifyError(err, host)
}

// hostOf extracts the host of a raw URL for an actionable, secret-free message (the
// host is non-secret config; a query string that might carry a secret is dropped).
func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host
	}
	return ""
}
