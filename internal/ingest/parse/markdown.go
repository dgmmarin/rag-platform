package parse

import (
	"strings"
)

// markdownParser handles text/markdown with line-based block detection: ATX
// headings, fenced code, pipe tables, lists and paragraphs (SPEC-05 §2 "Go
// passthrough, heading detection"). The chunker only needs block granularity and
// a heading tree, not rich inline semantics.
//
// ponytail: this is a line scanner, not a full CommonMark parser (no setext
// headings, no nested block quotes, no reference links). Ceiling: unusual
// markdown degrades to paragraphs, never to data loss. Upgrade path: swap in
// goldmark's AST walk if rich inline structure is ever needed downstream.
type markdownParser struct{}

func (markdownParser) Parse(data []byte) (Normalised, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	var (
		n     Normalised
		para  []string
		i     int
		flush = func() {}
	)
	flush = func() {
		if len(para) > 0 {
			n.Blocks = append(n.Blocks, Block{Type: Paragraph, Text: collapseWS(strings.Join(para, " "))})
			para = nil
		}
	}

	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			flush()
			i++

		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			flush()
			fence := trimmed[:3]
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence) {
				code = append(code, lines[i])
				i++
			}
			i++ // consume closing fence (or run off the end)
			n.Blocks = append(n.Blocks, Block{Type: Code, Text: strings.Join(code, "\n")})

		case headingLevel(trimmed) > 0:
			flush()
			level := headingLevel(trimmed)
			t := strings.TrimSpace(trimmed[level:])
			n.Blocks = append(n.Blocks, Block{Type: Heading, Level: level, Text: t})
			if n.Title == "" && level == 1 {
				n.Title = t
			}
			i++

		case isTableRow(trimmed) && i+1 < len(lines) && isTableSeparator(strings.TrimSpace(lines[i+1])):
			flush()
			var tbl []string
			for i < len(lines) && isTableRow(strings.TrimSpace(lines[i])) {
				tbl = append(tbl, strings.TrimSpace(lines[i]))
				i++
			}
			rows := parsePipeTable(tbl)
			n.Blocks = append(n.Blocks, Block{Type: Table, Rows: rows, Text: markdownTable(rows)})

		case isListItem(trimmed):
			flush()
			var items []string
			for i < len(lines) && isListItem(strings.TrimSpace(lines[i])) {
				items = append(items, strings.TrimSpace(lines[i]))
				i++
			}
			n.Blocks = append(n.Blocks, Block{Type: List, Text: strings.Join(items, "\n")})

		default:
			para = append(para, trimmed)
			i++
		}
	}
	flush()
	return n, nil
}

// headingLevel returns 1-6 for an ATX heading line ("## Title"), else 0.
func headingLevel(s string) int {
	level := 0
	for level < len(s) && s[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(s) || s[level] != ' ' {
		return 0
	}
	return level
}

func isListItem(s string) bool {
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") || strings.HasPrefix(s, "+ ") {
		return true
	}
	// ordered: "<digits>. "
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(s) && s[i] == '.' && s[i+1] == ' '
}

func isTableRow(s string) bool {
	return strings.Contains(s, "|")
}

// isTableSeparator matches the "|---|:--:|" delimiter row under a table header.
func isTableSeparator(s string) bool {
	if !strings.Contains(s, "-") {
		return false
	}
	for _, r := range s {
		switch r {
		case '|', '-', ':', ' ':
		default:
			return false
		}
	}
	return true
}

// parsePipeTable turns markdown table lines (header + separator + body) into
// structured rows, dropping the separator row.
func parsePipeTable(lines []string) [][]string {
	var rows [][]string
	for idx, l := range lines {
		if idx == 1 && isTableSeparator(l) {
			continue
		}
		rows = append(rows, splitPipeRow(l))
	}
	return rows
}

func splitPipeRow(l string) []string {
	l = strings.TrimSpace(l)
	l = strings.TrimPrefix(l, "|")
	l = strings.TrimSuffix(l, "|")
	parts := strings.Split(l, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}
