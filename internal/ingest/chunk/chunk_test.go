package chunk

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/ingest/parse"
)

func h(level int, text string) parse.Block {
	return parse.Block{Type: parse.Heading, Level: level, Text: text}
}
func p(text string) parse.Block { return parse.Block{Type: parse.Paragraph, Text: text} }

func TestHeadingPathRespected(t *testing.T) {
	doc := parse.Normalised{Title: "Guide", Blocks: []parse.Block{
		h(1, "Guide"), p("intro"),
		h(2, "Install"), p("step one"),
		h(3, "Linux"), p("linux step"),
		h(2, "Uninstall"), p("remove it"),
	}}
	got := Document(doc, Config{})

	want := []struct {
		path    []string
		content string
	}{
		{[]string{"Guide"}, "intro"},
		{[]string{"Guide", "Install"}, "step one"},
		{[]string{"Guide", "Install", "Linux"}, "linux step"},
		{[]string{"Guide", "Uninstall"}, "remove it"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Content != w.content {
			t.Errorf("chunk %d content = %q, want %q", i, got[i].Content, w.content)
		}
		if strings.Join(got[i].HeadingPath, ">") != strings.Join(w.path, ">") {
			t.Errorf("chunk %d heading_path = %v, want %v", i, got[i].HeadingPath, w.path)
		}
		if got[i].Position != i {
			t.Errorf("chunk %d position = %d", i, got[i].Position)
		}
	}
}

func TestContextLinePrependedToEmbedOnly(t *testing.T) {
	doc := parse.Normalised{Title: "Deployment Guide", Blocks: []parse.Block{
		h(1, "Overview"), h(2, "Prereqs"), p("You need Docker."),
	}}
	c := Document(doc, Config{})[0]

	if c.Content != "You need Docker." {
		t.Fatalf("content = %q; must not carry the context line", c.Content)
	}
	wantPrefix := "Deployment Guide > Overview > Prereqs\n"
	if !strings.HasPrefix(c.EmbedText, wantPrefix) {
		t.Errorf("embed text = %q, want prefix %q", c.EmbedText, wantPrefix)
	}
	if !strings.HasSuffix(c.EmbedText, c.Content) {
		t.Errorf("embed text should end with the stored content: %q", c.EmbedText)
	}
}

func TestContextLineDedupesTitleEqualHeading(t *testing.T) {
	// title == H1: the context line must not read "Guide > Guide".
	c := Document(parse.Normalised{Title: "Guide", Blocks: []parse.Block{h(1, "Guide"), p("body")}}, Config{})[0]
	if got := strings.SplitN(c.EmbedText, "\n", 2)[0]; got != "Guide" {
		t.Errorf("context line = %q, want %q", got, "Guide")
	}
}

func TestTableAndCodeKeptIntact(t *testing.T) {
	rows := [][]string{{"Provider", "Dim"}, {"OpenAI", "1536"}, {"Voyage", "1024"}}
	doc := parse.Normalised{Blocks: []parse.Block{
		p("Providers below."),
		{Type: parse.Table, Rows: rows, Text: parse.RenderTable(rows)},
		{Type: parse.Code, Text: "func main() {\n\tprintln(\"hi\")\n}"},
	}}
	got := Document(doc, Config{})
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3 (prose, table, code): %+v", len(got), got)
	}
	if got[1].Content != parse.RenderTable(rows) {
		t.Errorf("table chunk not intact:\n%s", got[1].Content)
	}
	if !strings.Contains(got[2].Content, "println(\"hi\")") || !strings.Contains(got[2].Content, "func main()") {
		t.Errorf("code chunk not intact:\n%s", got[2].Content)
	}
}

func TestTargetAndOverlapConfigurable(t *testing.T) {
	// Five paragraphs of ~6 tokens each; a 12-token target forces several chunks.
	var blocks []parse.Block
	for _, w := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		blocks = append(blocks, p(strings.TrimSpace(strings.Repeat(w+" ", 6))))
	}
	cfg := Config{TargetTokens: 12, OverlapTokens: 3}
	got := Document(parse.Normalised{Blocks: blocks}, cfg)
	if len(got) < 2 {
		t.Fatalf("expected multiple chunks under a small target, got %d", len(got))
	}
	// Overlap: each chunk after the first begins with the tail of its predecessor.
	for i := 1; i < len(got); i++ {
		tail := tailTokens(got[i-1].Content, cfg.OverlapTokens, ApproxTokens)
		if tail == "" || !strings.HasPrefix(got[i].Content, tail) {
			t.Errorf("chunk %d %q does not begin with overlap tail %q of chunk %d", i, got[i].Content, tail, i-1)
		}
	}
}

// TestSizeBoundsProperty is the story's headline property test: with every input
// block below the target, no chunk (overlap included) exceeds the target, and
// positions are dense and ordered.
func TestSizeBoundsProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cfg := Config{TargetTokens: 50, OverlapTokens: 8}
	for iter := 0; iter < 50; iter++ {
		var blocks []parse.Block
		n := rng.Intn(120) + 1
		for i := 0; i < n; i++ {
			words := rng.Intn(20) + 1 // <= 20 tokens, well under the 50 target
			blocks = append(blocks, p(strings.TrimSpace(strings.Repeat("word ", words))))
		}
		got := Document(parse.Normalised{Title: "T", Blocks: blocks}, cfg)
		for i, c := range got {
			if c.TokenCount > cfg.TargetTokens {
				t.Fatalf("iter %d chunk %d: %d tokens > target %d", iter, i, c.TokenCount, cfg.TargetTokens)
			}
			if c.TokenCount != ApproxTokens(c.Content) {
				t.Fatalf("iter %d chunk %d: TokenCount %d != recomputed %d", iter, i, c.TokenCount, ApproxTokens(c.Content))
			}
			if c.Position != i {
				t.Fatalf("iter %d: positions not dense at %d (got %d)", iter, i, c.Position)
			}
		}
	}
}

func TestOversizeTableSplitByRows(t *testing.T) {
	rows := [][]string{{"A", "B"}}
	for i := 0; i < 40; i++ {
		rows = append(rows, []string{"x", "y"})
	}
	full := parse.RenderTable(rows)
	cfg := Config{TargetTokens: 5} // 2*target = 10 < table tokens => must split
	got := Document(parse.Normalised{Blocks: []parse.Block{{Type: parse.Table, Rows: rows, Text: full}}}, cfg)
	if len(got) < 2 {
		t.Fatalf("oversize table not split: %d chunks", len(got))
	}
	for i, c := range got {
		if !strings.HasPrefix(c.Content, "| A | B |") {
			t.Errorf("part %d missing repeated header row:\n%s", i, c.Content)
		}
		for _, line := range strings.Split(c.Content, "\n") {
			if strings.Count(line, "|") != 3 { // "| _ | _ |" => 3 pipes; no broken row
				t.Errorf("part %d has a broken row: %q", i, line)
			}
		}
	}
}

func TestOversizeCodeSplitByLines(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, "line"+string(rune('a'+i%26)))
	}
	code := strings.Join(lines, "\n")
	cfg := Config{TargetTokens: 3}
	got := Document(parse.Normalised{Blocks: []parse.Block{{Type: parse.Code, Text: code}}}, cfg)
	if len(got) < 2 {
		t.Fatalf("oversize code not split: %d chunks", len(got))
	}
	var rebuilt []string
	for _, c := range got {
		rebuilt = append(rebuilt, strings.Split(c.Content, "\n")...)
	}
	if strings.Join(rebuilt, "\n") != code {
		t.Errorf("code lines not preserved in order:\n%s", strings.Join(rebuilt, "\n"))
	}
}

func TestEmptyAndBlankBlocks(t *testing.T) {
	got := Document(parse.Normalised{Blocks: []parse.Block{p("  "), h(1, "X"), p("\n\n")}}, Config{})
	if len(got) != 0 {
		t.Fatalf("blank blocks produced %d chunks: %+v", len(got), got)
	}
}

func TestApproxTokens(t *testing.T) {
	if n := ApproxTokens("hello, world!"); n != 4 { // hello , world !
		t.Errorf("ApproxTokens = %d, want 4", n)
	}
	if ApproxTokens("   ") != 0 {
		t.Error("whitespace should be 0 tokens")
	}
}
