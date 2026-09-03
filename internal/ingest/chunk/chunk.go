// Package chunk splits a parsed document (parse.Normalised) into retrieval
// chunks (SPEC-05 §3, FR-ING-03/04). It walks the blocks maintaining a
// heading_path, accumulates prose up to a token target, keeps tables and code
// blocks intact, carries a token overlap between consecutive prose chunks, and
// prepends a per-chunk context line to the text that will be embedded (but not to
// the text that is stored). It is pure: blocks in, chunks out, no I/O.
//
// The output Chunk carries both Content (stored verbatim in chunks.content) and
// EmbedText (Content prefixed with "{title} > {heading path}"): the embedder
// (STORY-05.5) embeds EmbedText, the store (STORY-05.1) persists Content. The
// sink (STORY-05.6) maps a Chunk plus its embedding to a documents.ChunkInput.
package chunk

import (
	"regexp"
	"strings"

	"github.com/rag-platform/ragctl/internal/ingest/parse"
)

// Defaults from SPEC-05 §3.
const (
	DefaultTargetTokens  = 512
	DefaultOverlapTokens = 64
)

// Chunk is one unit of retrievable content.
type Chunk struct {
	// Position is the 0-based order within the document (chunks.position).
	Position int
	// HeadingPath is the heading stack above this chunk, e.g. ["Install", "Linux"]
	// (chunks.heading_path).
	HeadingPath []string
	// Content is the stored text (chunks.content) — never carries the context line.
	Content string
	// EmbedText is what the embedder should embed: Content prefixed with a context
	// line "{title} > {heading path}" (SPEC-05 §3), improving retrieval of short
	// chunks. Not stored.
	EmbedText string
	// TokenCount is the token estimate of Content (chunks.token_count).
	TokenCount int
}

// Config tunes the split. TargetTokens/OverlapTokens come from tenant settings at
// runtime (STORY-05.6); zero values fall back to the SPEC-05 §3 defaults. Count is
// the token counter — injectable so a real tiktoken cl100k_base counter can
// replace the default approximation (SPEC-05 §3 permits an approximation).
type Config struct {
	TargetTokens  int
	OverlapTokens int
	Count         func(string) int
}

func (c Config) withDefaults() Config {
	if c.TargetTokens <= 0 {
		c.TargetTokens = DefaultTargetTokens
	}
	if c.OverlapTokens < 0 {
		c.OverlapTokens = 0
	}
	if c.OverlapTokens == 0 {
		c.OverlapTokens = DefaultOverlapTokens
	}
	// Overlap must be strictly less than the target or a chunk could be seeded to
	// its own limit and never accept content (fail closed to a sane split).
	if c.OverlapTokens >= c.TargetTokens {
		c.OverlapTokens = c.TargetTokens / 2
	}
	if c.Count == nil {
		c.Count = ApproxTokens
	}
	return c
}

// tokenRE approximates a BPE tokeniser: each run of alphanumerics and each
// individual non-space symbol is one token.
//
// ponytail: this UNDERcounts long words a real BPE would split into sub-word
// pieces (cl100k_base). Ceiling: chunks may run somewhat larger in true model
// tokens than the target. Upgrade path: inject a tiktoken cl100k_base counter via
// Config.Count — the split logic is unchanged.
var tokenRE = regexp.MustCompile(`[\p{L}\p{N}]+|[^\s\p{L}\p{N}]`)

// ApproxTokens is the default token estimate (SPEC-05 §3: approximation ok).
func ApproxTokens(s string) int { return len(tokenRE.FindAllString(s, -1)) }

// Document splits n into chunks per SPEC-05 §3.
func Document(n parse.Normalised, cfg Config) []Chunk {
	cfg = cfg.withDefaults()
	s := &splitter{cfg: cfg, title: strings.TrimSpace(n.Title)}
	for _, b := range n.Blocks {
		s.block(b)
	}
	s.flushProse()
	return s.out
}

type splitter struct {
	cfg     Config
	title   string
	path    []string
	buf     []string // accumulated prose block texts for the pending chunk
	bufToks int
	out     []Chunk
	pos     int
}

func (s *splitter) block(b parse.Block) {
	switch b.Type {
	case parse.Heading:
		// A heading ends the current section's chunk and re-roots the path so every
		// chunk carries exactly one heading_path (AC: respects headings).
		s.flushProse()
		level := b.Level
		if level < 1 {
			level = 1
		}
		if level > 6 {
			level = 6
		}
		if level-1 < len(s.path) {
			s.path = s.path[:level-1]
		} else {
			for len(s.path) < level-1 {
				s.path = append(s.path, "")
			}
		}
		s.path = append(s.path, strings.TrimSpace(b.Text))

	case parse.Table, parse.Code:
		// Atomic: never interleaved with prose, never split between rows/lines.
		s.flushProse()
		s.emitAtomic(b)

	default: // Paragraph, List
		t := strings.TrimSpace(b.Text)
		if t == "" {
			return
		}
		toks := s.cfg.Count(t)
		if s.bufToks > 0 && s.bufToks+toks > s.cfg.TargetTokens {
			prev := s.currentContent()
			s.flushProse()
			if tail := tailTokens(prev, s.cfg.OverlapTokens, s.cfg.Count); tail != "" {
				s.buf = append(s.buf, tail)
				s.bufToks = s.cfg.Count(tail)
			}
		}
		s.buf = append(s.buf, t)
		s.bufToks += toks
	}
}

func (s *splitter) currentContent() string { return strings.Join(s.buf, "\n\n") }

func (s *splitter) flushProse() {
	if len(s.buf) == 0 {
		return
	}
	s.emit(s.currentContent())
	s.buf = nil
	s.bufToks = 0
}

// emitAtomic emits a table/code block whole, or — only when it alone exceeds twice
// the target (SPEC-05 §3) — split between rows/lines so no row or line is broken.
func (s *splitter) emitAtomic(b parse.Block) {
	text := strings.TrimSpace(b.Text)
	if text == "" {
		return
	}
	if s.cfg.Count(text) <= 2*s.cfg.TargetTokens {
		s.emit(text)
		return
	}
	for _, part := range s.splitOversize(b) {
		s.emit(part)
	}
}

// splitOversize breaks an over-2×-target atomic block on safe boundaries: table
// rows (header repeated on each part) or code lines.
func (s *splitter) splitOversize(b parse.Block) []string {
	if b.Type == parse.Table && len(b.Rows) > 1 {
		header := b.Rows[0]
		var parts []string
		group := [][]string{header}
		for _, row := range b.Rows[1:] {
			group = append(group, row)
			if s.cfg.Count(parse.RenderTable(group)) >= s.cfg.TargetTokens {
				parts = append(parts, parse.RenderTable(group))
				group = [][]string{header}
			}
		}
		if len(group) > 1 {
			parts = append(parts, parse.RenderTable(group))
		}
		if len(parts) > 0 {
			return parts
		}
	}
	// Code, or a table we could not row-split: break on lines.
	lines := strings.Split(strings.TrimSpace(b.Text), "\n")
	var parts []string
	var cur []string
	curToks := 0
	for _, ln := range lines {
		lt := s.cfg.Count(ln)
		if len(cur) > 0 && curToks+lt > s.cfg.TargetTokens {
			parts = append(parts, strings.Join(cur, "\n"))
			cur, curToks = nil, 0
		}
		cur = append(cur, ln)
		curToks += lt
	}
	if len(cur) > 0 {
		parts = append(parts, strings.Join(cur, "\n"))
	}
	return parts
}

func (s *splitter) emit(content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	path := append([]string(nil), s.path...)
	s.out = append(s.out, Chunk{
		Position:    s.pos,
		HeadingPath: path,
		Content:     content,
		EmbedText:   withContext(s.title, path, content),
		TokenCount:  s.cfg.Count(content),
	})
	s.pos++
}

// withContext prepends the SPEC-05 §3 context line "{title} > {heading path}" to
// the text that will be embedded. Empty title/path segments are dropped; with
// neither, the content is embedded as-is.
func withContext(title string, path []string, content string) string {
	parts := make([]string, 0, len(path)+1)
	push := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || (len(parts) > 0 && parts[len(parts)-1] == v) {
			return // drop empties and a segment equal to the previous (title == H1)
		}
		parts = append(parts, v)
	}
	push(title)
	for _, h := range path {
		push(h)
	}
	if len(parts) == 0 {
		return content
	}
	return strings.Join(parts, " > ") + "\n" + content
}

// tailTokens returns the trailing whitespace-delimited words of s whose combined
// estimate is at least n tokens — the overlap carried into the next chunk.
//
// ponytail: word-granular overlap (not exact BPE-token granular). Ceiling: the
// carried overlap is approximate; acceptable per SPEC-05 §3.
func tailTokens(s string, n int, count func(string) int) string {
	if n <= 0 {
		return ""
	}
	words := strings.Fields(s)
	total, start := 0, len(words)
	for i := len(words) - 1; i >= 0; i-- {
		total += count(words[i])
		start = i
		if total >= n {
			break
		}
	}
	return strings.Join(words[start:], " ")
}
