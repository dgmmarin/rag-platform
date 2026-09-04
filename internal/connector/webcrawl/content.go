package webcrawl

import (
	"bytes"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"

	"github.com/rag-platform/ragctl/internal/ingest/parse"
)

// mdRegistry renders cleaned HTML into the SPEC-05 §2 Normalised representation and
// thence to markdown. It reuses the platform's existing semantic HTML→markdown
// engine (parse.htmlParser, ADR-0034) rather than adding a readability/
// html-to-markdown dependency — ADR-0034 deliberately rejected both (a maintained
// readability port forces a newer Go toolchain than the repo pins; an
// html-to-markdown library collapses the heading/table structure the pipeline
// needs). Registry.Parse only reads the parser map, so concurrent use across crawl
// goroutines is safe.
var mdRegistry = parse.Default()

// extractContent turns one HTML page into the clean markdown body a connector.
// Document carries for HTML (SPEC-04 §2, STORY-07.3). It applies the per-source
// include/exclude CSS selectors (FR-SRC-05) and falls back to the readability-style
// semantic extraction (ADR-0034) when no include selectors are configured or they
// select nothing usable, then renders the result to markdown.
//
// Extraction order:
//  1. exclude_selectors always drop their matching subtrees (nav/footer/cookie/ad …).
//  2. include_selectors, when configured, keep ONLY their top-most matching
//     subtrees; anything outside is discarded.
//  3. with no usable include match, the whole (exclude-applied) document is handed
//     to the readability heuristic, which selects the <main>/<article>/<body>
//     content root and skips known chrome.
//
// It returns "" only when parsing fails (a non-HTML body, or unparseable bytes);
// the caller then falls back to emitting the raw body.
func extractContent(body []byte, include, exclude []string) string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return ""
	}

	// (1) exclude: remove every matching subtree from the document.
	for _, sel := range exclude {
		c, err := cascadia.Compile(sel)
		if err != nil {
			continue // a malformed selector drops out of the pipeline, not the page
		}
		for _, n := range c.MatchAll(doc) {
			if n.Parent != nil {
				n.Parent.RemoveChild(n)
			}
		}
	}

	// (2) include: keep only the top-most matching subtrees, if any match.
	var contentHTML []byte
	if matches := topMatches(doc, include); len(matches) > 0 {
		var buf bytes.Buffer
		buf.WriteString("<html><body>")
		for _, m := range matches {
			_ = html.Render(&buf, m)
		}
		buf.WriteString("</body></html>")
		contentHTML = buf.Bytes()
	} else {
		// (3) readability fallback: render the whole (exclude-applied) doc and let
		// the semantic htmlParser pick the content root and skip chrome.
		var buf bytes.Buffer
		if err := html.Render(&buf, doc); err != nil {
			return ""
		}
		contentHTML = buf.Bytes()
	}

	norm, err := mdRegistry.Parse("text/html", contentHTML)
	if err != nil {
		return ""
	}
	return norm.Markdown()
}

// topMatches returns the top-most nodes matching any of the include selectors, in
// document order, with matches nested inside another match removed so a subtree is
// never rendered twice. An empty or all-invalid selector list yields no matches
// (the caller then uses the readability fallback).
func topMatches(doc *html.Node, include []string) []*html.Node {
	var all []*html.Node
	seen := map[*html.Node]bool{}
	for _, sel := range include {
		c, err := cascadia.Compile(sel)
		if err != nil {
			continue
		}
		for _, n := range c.MatchAll(doc) {
			if !seen[n] {
				seen[n] = true
				all = append(all, n)
			}
		}
	}
	var top []*html.Node
	for _, n := range all {
		if !hasAncestorIn(n, seen) {
			top = append(top, n)
		}
	}
	return top
}

// hasAncestorIn reports whether any ancestor of n is itself a match (in set).
func hasAncestorIn(n *html.Node, set map[*html.Node]bool) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if set[p] {
			return true
		}
	}
	return false
}
