package webcrawl

import (
	"fmt"
	"net/url"
	"strings"
)

// trackingParams are query keys stripped during normalisation so that two URLs
// differing only by campaign/click tracking normalise to the same key (SPEC-04
// §2 "strip fragments and tracking/utm params"). Any key with the "utm_" prefix
// is stripped in addition to this fixed set.
//
// ponytail: a small hand-maintained tracker set covers the common cases; the
// upgrade path is a configurable strip-list per source if a tenant needs one.
var trackingParams = map[string]bool{
	"gclid":   true,
	"fbclid":  true,
	"mc_cid":  true,
	"mc_eid":  true,
	"yclid":   true,
	"msclkid": true,
	"igshid":  true,
	"_hsenc":  true,
	"_hsmi":   true,
	"ref_src": true,
}

// parseAndNormalize parses a raw absolute URL and returns its canonical form plus
// the parsed *url.URL (pre-normalisation, useful as a base for relative links).
// Only http/https are accepted; other schemes (mailto, javascript, ftp, …) are
// rejected so the crawler never enqueues a non-fetchable URI.
func parseAndNormalize(raw string) (string, *url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", nil, fmt.Errorf("webcrawl: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", nil, fmt.Errorf("webcrawl: unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", nil, fmt.Errorf("webcrawl: url has no host: %q", raw)
	}
	return normalize(u), u, nil
}

// normalize returns the canonical string form of an absolute http(s) URL: scheme
// and host lowercased, default port removed, fragment dropped, tracking/utm query
// params stripped and the remaining params sorted by key (SPEC-04 §2). The path is
// left as-is (case preserved), with an empty path canonicalised to "/".
func normalize(in *url.URL) string {
	u := *in // copy; never mutate the caller's URL
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	// Drop the default port for the scheme (Hostname()+Port() splits safely).
	if port := u.Port(); port != "" {
		if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
			u.Host = u.Hostname()
		}
	}
	u.Fragment = ""
	u.RawFragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	if u.RawQuery != "" {
		q := u.Query()
		for key := range q {
			if strings.HasPrefix(strings.ToLower(key), "utm_") || trackingParams[strings.ToLower(key)] {
				delete(q, key)
			}
		}
		// url.Values.Encode sorts by key, giving a stable canonical query string.
		u.RawQuery = q.Encode()
	}
	return u.String()
}
