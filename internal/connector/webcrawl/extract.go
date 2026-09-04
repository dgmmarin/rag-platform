package webcrawl

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// extracted is the minimal structural view the CRAWLER needs from an HTML page:
// the title (for the Document title), the canonical URL (SPEC-04 §2: becomes the
// Document ExternalID) and the outbound links (the BFS frontier). It deliberately
// does NOT include readable body text.
//
// EXTRACTION SEAM (STORY-07.3): high-quality content extraction —
// readability-style boilerplate removal, include/exclude selectors, and
// HTML→markdown — is a later story. For STORY-07.1 the crawler emits the RAW page
// bytes as the Document Body (SPEC-04 §2), and 07.3 will slot its extractor in
// behind this same parse step without touching crawl logic.
type extracted struct {
	title     string
	canonical string   // normalised absolute canonical URL, or "" if none/invalid
	links     []string // normalised absolute http(s) links, deduped
}

// extractHTML parses an HTML document and pulls the crawl-relevant structure.
// Links and the canonical href are resolved against base and normalised; non-HTTP
// or unparseable references are dropped (allow/deny/robots gating happens later in
// the crawler, not here).
func extractHTML(body []byte, base *url.URL) (extracted, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return extracted{}, err
	}
	var ex extracted
	seen := map[string]bool{}
	var inTitle bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				inTitle = true
				defer func() { inTitle = false }()
			case "link":
				if attr(n, "rel") == "canonical" {
					if norm, ok := resolveNormalize(base, attr(n, "href")); ok {
						ex.canonical = norm
					}
				}
			case "a":
				if href := attr(n, "href"); href != "" {
					if norm, ok := resolveNormalize(base, href); ok && !seen[norm] {
						seen[norm] = true
						ex.links = append(ex.links, norm)
					}
				}
			}
		}
		if inTitle && n.Type == html.TextNode && ex.title == "" {
			ex.title = strings.TrimSpace(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return ex, nil
}

// resolveNormalize resolves ref against base and returns its normalised form.
// ok is false for empty, non-HTTP, in-page, or unparseable references.
func resolveNormalize(base *url.URL, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") {
		return "", false
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", false
	}
	abs := base.ResolveReference(u) // resolves relative paths and dot-segments
	norm, _, err := parseAndNormalize(abs.String())
	if err != nil {
		return "", false
	}
	return norm, true
}

// attr returns the value of the named attribute, or "" if absent (case-insensitive
// on the attribute key, which the html tokenizer already lowercases).
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
