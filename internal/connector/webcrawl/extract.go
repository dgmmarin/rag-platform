package webcrawl

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// extracted is the structural view the CRAWLER needs from an HTML page: the title
// (for the Document title), the canonical URL (SPEC-04 §2: becomes the Document
// ExternalID) and the outbound links (the BFS frontier). It deliberately does NOT
// include the readable body markdown — that is produced separately by
// extractContent (STORY-07.3, content.go), which the selector/readability
// extraction operates over.
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
	// Title precedence (SPEC-04 §2): <title> → og:title → first <h1>. All three
	// candidates are collected in one pass; precedence is resolved at the end.
	var titleTag, ogTitle, firstH1 string
	var inTitle, inH1 bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				inTitle = true
				defer func() { inTitle = false }()
			case "meta":
				if firstH1 == "" && ogTitle == "" && attr(n, "property") == "og:title" {
					ogTitle = strings.TrimSpace(attr(n, "content"))
				}
			case "h1":
				inH1 = true
				defer func() { inH1 = false }()
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
		if n.Type == html.TextNode {
			if inTitle && titleTag == "" {
				titleTag = strings.TrimSpace(n.Data)
			}
			if inH1 && firstH1 == "" {
				firstH1 = strings.TrimSpace(n.Data)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	ex.title = firstNonEmpty(titleTag, ogTitle, firstH1)
	return ex, nil
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
