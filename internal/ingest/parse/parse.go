// Package parse turns the native-Go document formats (HTML, Markdown, plain
// text, CSV, JSON) into the SPEC-05 §2 normalised representation the ingestion
// pipeline consumes. The heavy formats (PDF/DOCX/PPTX/XLSX) are handled by the
// Python sidecar (ADR-0006, STORY-05.3); this package is the Go half.
//
// The output type Normalised mirrors the sidecar's JSON response
// ({title, blocks:[{type, level, text, rows}]}) exactly, so a Go parser and the
// sidecar feed the same downstream stages: the normalise/hash step (SPEC-05 §1)
// and the structure-aware chunker (SPEC-05 §3, STORY-05.4), which walks Blocks
// maintaining a heading_path. Nothing here touches tenant storage or the network;
// these are pure functions (FR-ING-01).
package parse

import (
	"errors"
	"fmt"
	"mime"
	"strings"
)

// ErrUnsupportedMIME is returned by Registry.Parse for a MIME type no parser is
// registered for (e.g. a sidecar format, or an unknown type). Callers match with
// errors.Is.
var ErrUnsupportedMIME = errors.New("parse: unsupported mime type")

// BlockType is one of the SPEC-05 §2 block kinds. It is the same closed set the
// sidecar emits so both producers are interchangeable to the chunker.
type BlockType string

const (
	Heading   BlockType = "heading"
	Paragraph BlockType = "paragraph"
	Table     BlockType = "table"
	List      BlockType = "list"
	Code      BlockType = "code"
)

// Block is one structural unit of a parsed document (SPEC-05 §2).
type Block struct {
	Type BlockType `json:"type"`
	// Level is the heading level (1-6) for Heading blocks; 0 otherwise.
	Level int `json:"level,omitempty"`
	// Text is the markdown-rendered content of the block. For a Table it is the
	// GFM markdown table (SPEC-05 §1/§2 "tables as markdown"); for Code it is the
	// raw code body; for the rest it is inline markdown text.
	Text string `json:"text,omitempty"`
	// Rows carries the structured cells of a Table block (header row first),
	// matching the sidecar's `rows`. Empty for every other block type.
	Rows [][]string `json:"rows,omitempty"`
}

// Normalised is the parser output: a title plus an ordered list of blocks
// (SPEC-05 §1 Normalised{Title, Blocks}). Title may be empty when the format
// carries none in its content (plain text, CSV) — document-level title then comes
// from source metadata, not the body.
type Normalised struct {
	Title  string  `json:"title"`
	Blocks []Block `json:"blocks"`
}

// Markdown renders the blocks back to a single markdown document. This is the
// "normalised text" the SPEC-05 §1 flow hashes (FR-ING-02) and the human-readable
// content a version stores. Title is metadata and is not prepended here.
func (n Normalised) Markdown() string {
	parts := make([]string, 0, len(n.Blocks))
	for _, b := range n.Blocks {
		switch b.Type {
		case Heading:
			level := b.Level
			if level < 1 {
				level = 1
			}
			if level > 6 {
				level = 6
			}
			parts = append(parts, strings.Repeat("#", level)+" "+b.Text)
		case Code:
			parts = append(parts, "```\n"+b.Text+"\n```")
		default: // Paragraph, List, Table already carry ready markdown in Text.
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// Parser turns raw bytes of one format into a Normalised document. Implementations
// are pure and stateless; a document is small (bounded by MAX_UPLOAD_BYTES,
// STORY-04.4) so the whole body is passed in memory rather than streamed.
type Parser interface {
	Parse(data []byte) (Normalised, error)
}

// Registry maps a canonical MIME type to its Parser (SPEC-05 §2). It is the only
// dispatch point; the caller passes the MIME resolved from the upload allowlist /
// content-type detection (STORY-04.4) — parsers never sniff.
type Registry struct {
	parsers map[string]Parser
}

// NewRegistry returns an empty registry. Use Default for the standard Go parser
// set.
func NewRegistry() *Registry {
	return &Registry{parsers: map[string]Parser{}}
}

// Register binds a parser to a canonical MIME type (parameters like "; charset"
// must not be present here; Parse strips them from the lookup key). A second
// Register for the same type overwrites the first.
func (r *Registry) Register(mimeType string, p Parser) {
	r.parsers[mimeType] = p
}

// Parse dispatches to the parser for contentType's base MIME type. Parameters
// (charset, boundary, …) are stripped before lookup. Returns ErrUnsupportedMIME
// when no parser is registered.
func (r *Registry) Parse(contentType string, data []byte) (Normalised, error) {
	base := canonicalMIME(contentType)
	p, ok := r.parsers[base]
	if !ok {
		return Normalised{}, fmt.Errorf("%w: %q", ErrUnsupportedMIME, base)
	}
	return p.Parse(data)
}

// Default returns a registry with every native-Go parser registered under its
// canonical MIME type (SPEC-05 §2). The sidecar formats are intentionally absent.
func Default() *Registry {
	r := NewRegistry()
	r.Register("text/html", htmlParser{})
	r.Register("text/markdown", markdownParser{})
	r.Register("text/plain", textParser{})
	r.Register("text/csv", csvParser{})
	r.Register("application/json", jsonParser{})
	return r
}

// canonicalMIME lowercases and strips parameters from a content-type header
// ("text/html; charset=utf-8" -> "text/html"). It falls back to a trimmed,
// lower-cased string when the header cannot be parsed.
func canonicalMIME(contentType string) string {
	if base, _, err := mime.ParseMediaType(contentType); err == nil {
		return base
	}
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

// RenderTable renders rows (header row first) as a GFM markdown table — the same
// rendering the parsers use for Table blocks. Exported for the chunker (STORY-05.4),
// which re-renders header-plus-rows parts when it must split an oversize table
// (SPEC-05 §3) without breaking a row.
func RenderTable(rows [][]string) string { return markdownTable(rows) }

// markdownTable renders rows (header first) as a GFM markdown table. Pipes inside
// cells are escaped. An empty input yields an empty string. It is shared by the
// HTML, CSV, Markdown and JSON parsers so every "table as markdown" is identical.
func markdownTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return ""
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteByte('|')
		for c := 0; c < cols; c++ {
			cell := ""
			if c < len(cells) {
				cell = strings.ReplaceAll(cells[c], "|", "\\|")
			}
			b.WriteByte(' ')
			b.WriteString(cell)
			b.WriteString(" |")
		}
	}
	writeRow(rows[0])
	b.WriteByte('\n')
	b.WriteByte('|')
	for c := 0; c < cols; c++ {
		b.WriteString(" --- |")
	}
	for _, r := range rows[1:] {
		b.WriteByte('\n')
		writeRow(r)
	}
	return b.String()
}

// collapseWS collapses every run of whitespace (including newlines) to a single
// space and trims the ends — the normalisation inline text needs so hashing is
// stable regardless of source formatting.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
