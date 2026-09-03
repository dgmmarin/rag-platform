package parse

import (
	"bytes"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlParser handles text/html: strip site chrome (readability-style), then walk
// the main-content DOM into blocks, preserving headings and rendering tables as
// markdown (SPEC-05 §2, story AC "HTML boilerplate removed" / "headings preserved,
// tables → markdown").
//
// The boilerplate heuristic (ADR-0034) is deliberately semantic rather than the
// full Mozilla-Readability text-density scoring: choose the <main>/<article>
// subtree when present, else the <body>, and skip known-chrome subtrees
// (nav/aside/header/footer/script/style + boilerplate class/id/role). It is tested
// against representative pages via the golden fixtures.
//
// ponytail: no text-density scoring, so a page that buries its article in an
// unmarked <div> with sibling chrome divs keeps some chrome. Ceiling: extra
// paragraphs, never data loss. Upgrade path: add density scoring, or adopt a
// maintained readability library once one stops forcing a Go-toolchain bump.
type htmlParser struct{}

// boilerplateAttr matches class/id/role tokens that mark site chrome.
var boilerplateAttr = regexp.MustCompile(`(?i)\b(nav|navbar|menu|sidebar|footer|header|banner|masthead|breadcrumb|advert|\bad\b|ads|promo|sponsor|cookie|consent|comment|social|share|related|widget|newsletter|subscribe|skip-link|screen-reader)\b`)

func (htmlParser) Parse(data []byte) (Normalised, error) {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return Normalised{}, err
	}

	var n Normalised
	n.Title = collapseWS(findTitle(doc))

	root := contentRoot(doc)
	rootIsBody := root != nil && root.DataAtom == atom.Body
	if root == nil {
		return n, nil
	}
	walkBlocks(root, rootIsBody, &n.Blocks)

	// If chrome removal (or a chrome-only <main>) left nothing, fall back to the
	// whole body so we never drop a document's content entirely.
	if len(n.Blocks) == 0 {
		if body := firstElement(doc, atom.Body); body != nil && body != root {
			walkBlocks(body, true, &n.Blocks)
		}
	}
	if n.Title == "" {
		for _, b := range n.Blocks {
			if b.Type == Heading {
				n.Title = b.Text
				break
			}
		}
	}
	return n, nil
}

// contentRoot picks the main-content subtree: the first <main>, else the first
// <article>, else the <body>.
func contentRoot(doc *html.Node) *html.Node {
	if m := firstElement(doc, atom.Main); m != nil {
		return m
	}
	if a := firstElement(doc, atom.Article); a != nil {
		return a
	}
	return firstElement(doc, atom.Body)
}

// walkBlocks descends n in document order, emitting one Block per block-level
// element and skipping chrome subtrees.
func walkBlocks(n *html.Node, rootIsBody bool, out *[]Block) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		if isChrome(c, rootIsBody) {
			continue
		}
		switch c.DataAtom {
		case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
			if t := inlineMarkdown(c); t != "" {
				*out = append(*out, Block{Type: Heading, Level: headingRank(c.DataAtom), Text: t})
			}
		case atom.P:
			if t := inlineMarkdown(c); t != "" {
				*out = append(*out, Block{Type: Paragraph, Text: t})
			}
		case atom.Pre:
			if t := strings.Trim(textContent(c), "\n"); t != "" {
				*out = append(*out, Block{Type: Code, Text: t})
			}
		case atom.Ul, atom.Ol:
			if t := listMarkdown(c, c.DataAtom == atom.Ol); t != "" {
				*out = append(*out, Block{Type: List, Text: t})
			}
		case atom.Table:
			rows := tableRows(c)
			if len(rows) > 0 {
				*out = append(*out, Block{Type: Table, Rows: rows, Text: markdownTable(rows)})
			}
		default:
			// A container with block descendants: recurse. A container with only
			// inline/text content (e.g. <div>bare text</div>): capture as a
			// paragraph so nothing is lost.
			if hasBlockDescendant(c) {
				walkBlocks(c, rootIsBody, out)
			} else if t := inlineMarkdown(c); t != "" {
				*out = append(*out, Block{Type: Paragraph, Text: t})
			}
		}
	}
}

// isChrome reports whether a node is site boilerplate to be skipped. nav/aside and
// non-content tags are always chrome; header/footer are chrome only at the body
// level (inside <article>/<main> they are content-local).
func isChrome(n *html.Node, rootIsBody bool) bool {
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Svg,
		atom.Iframe, atom.Form, atom.Button, atom.Nav, atom.Aside:
		return true
	case atom.Header, atom.Footer:
		if rootIsBody {
			return true
		}
	}
	if v := attr(n, "role"); v != "" {
		switch strings.ToLower(v) {
		case "navigation", "banner", "contentinfo", "complementary", "search":
			return true
		}
	}
	return boilerplateAttr.MatchString(attr(n, "class")) || boilerplateAttr.MatchString(attr(n, "id"))
}

func headingRank(a atom.Atom) int {
	switch a {
	case atom.H1:
		return 1
	case atom.H2:
		return 2
	case atom.H3:
		return 3
	case atom.H4:
		return 4
	case atom.H5:
		return 5
	default:
		return 6
	}
}

// hasBlockDescendant reports whether n contains any block-level element, so the
// walker knows whether to recurse or treat n as a leaf paragraph.
func hasBlockDescendant(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		switch c.DataAtom {
		case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
			atom.P, atom.Pre, atom.Ul, atom.Ol, atom.Table,
			atom.Div, atom.Section, atom.Article, atom.Main, atom.Blockquote:
			return true
		}
		if hasBlockDescendant(c) {
			return true
		}
	}
	return false
}

// inlineMarkdown renders the inline content of a node to markdown: links, bold,
// italic and inline code, with whitespace collapsed.
func inlineMarkdown(n *html.Node) string {
	var b strings.Builder
	renderInline(n, &b)
	return collapseWS(b.String())
}

func renderInline(n *html.Node, b *strings.Builder) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			b.WriteString(c.Data)
		case html.ElementNode:
			switch c.DataAtom {
			case atom.Script, atom.Style, atom.Noscript:
				// drop
			case atom.A:
				inner := inlineMarkdown(c)
				href := attr(c, "href")
				if href == "" || inner == "" {
					b.WriteString(inner)
				} else {
					b.WriteString("[" + inner + "](" + href + ")")
				}
			case atom.Strong, atom.B:
				if inner := inlineMarkdown(c); inner != "" {
					b.WriteString("**" + inner + "**")
				}
			case atom.Em, atom.I:
				if inner := inlineMarkdown(c); inner != "" {
					b.WriteString("_" + inner + "_")
				}
			case atom.Code:
				if inner := collapseWS(textContent(c)); inner != "" {
					b.WriteString("`" + inner + "`")
				}
			case atom.Br:
				b.WriteByte(' ')
			case atom.Img:
				b.WriteString(attr(c, "alt"))
			default:
				renderInline(c, b)
			}
		}
	}
}

// listMarkdown renders <ul>/<ol> to markdown list lines. Nested lists are indented
// two spaces per level.
func listMarkdown(n *html.Node, ordered bool) string {
	var lines []string
	collectListItems(n, ordered, 0, &lines)
	return strings.Join(lines, "\n")
}

func collectListItems(list *html.Node, ordered bool, depth int, lines *[]string) {
	idx := 0
	for li := list.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode || li.DataAtom != atom.Li {
			continue
		}
		idx++
		marker := "- "
		if ordered {
			marker = itoa(idx) + ". "
		}
		// Item text is the inline content excluding nested lists (handled below as
		// indented sub-items).
		text := collapseWS(inlineOfChildrenExcludingLists(li))
		*lines = append(*lines, strings.Repeat("  ", depth)+marker+text)
		for c := li.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (c.DataAtom == atom.Ul || c.DataAtom == atom.Ol) {
				collectListItems(c, c.DataAtom == atom.Ol, depth+1, lines)
			}
		}
	}
}

// inlineOfChildrenExcludingLists renders an <li>'s inline content but skips nested
// <ul>/<ol> (handled separately as indented sub-items).
func inlineOfChildrenExcludingLists(li *html.Node) string {
	var b strings.Builder
	for c := li.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			b.WriteString(c.Data)
		case html.ElementNode:
			if c.DataAtom == atom.Ul || c.DataAtom == atom.Ol {
				continue
			}
			var one strings.Builder
			renderElementInline(c, &one)
			b.WriteString(one.String())
		}
	}
	return b.String()
}

// renderElementInline renders a single element node (and its subtree) inline,
// reusing the same rules as renderInline for the element itself.
func renderElementInline(c *html.Node, b *strings.Builder) {
	switch c.DataAtom {
	case atom.A:
		inner := inlineMarkdown(c)
		href := attr(c, "href")
		if href == "" || inner == "" {
			b.WriteString(inner)
		} else {
			b.WriteString("[" + inner + "](" + href + ")")
		}
	case atom.Strong, atom.B:
		if inner := inlineMarkdown(c); inner != "" {
			b.WriteString("**" + inner + "**")
		}
	case atom.Em, atom.I:
		if inner := inlineMarkdown(c); inner != "" {
			b.WriteString("_" + inner + "_")
		}
	case atom.Code:
		if inner := collapseWS(textContent(c)); inner != "" {
			b.WriteString("`" + inner + "`")
		}
	default:
		renderInline(c, b)
	}
}

// tableRows extracts a table's cells as rows (header row first), reading th/td in
// document order across thead/tbody/tr.
func tableRows(table *html.Node) [][]string {
	var rows [][]string
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			if c.DataAtom == atom.Tr {
				var cells []string
				for cell := c.FirstChild; cell != nil; cell = cell.NextSibling {
					if cell.Type == html.ElementNode && (cell.DataAtom == atom.Td || cell.DataAtom == atom.Th) {
						cells = append(cells, inlineMarkdown(cell))
					}
				}
				if len(cells) > 0 {
					rows = append(rows, cells)
				}
			} else {
				visit(c)
			}
		}
	}
	visit(table)
	return rows
}

// findTitle returns the <title> text, else "".
func findTitle(doc *html.Node) string {
	t := firstElement(doc, atom.Title)
	if t == nil {
		return ""
	}
	return textContent(t)
}

// firstElement returns the first element with the given atom in document order.
func firstElement(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := firstElement(c, a); found != nil {
			return found
		}
	}
	return nil
}

// textContent returns the concatenated text of a subtree, preserving whitespace
// (used for <pre>/<code>).
func textContent(n *html.Node) string {
	var b strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(n)
	return b.String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
