package parse

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update regenerates the golden files (`go test ./internal/ingest/parse -update`).
var update = flag.Bool("update", false, "update golden files")

// extMIME maps a fixture extension to the canonical MIME the registry is keyed by
// (SPEC-05 §2). It mirrors the upload allowlist canonicalisation used elsewhere.
var extMIME = map[string]string{
	".html": "text/html",
	".md":   "text/markdown",
	".txt":  "text/plain",
	".csv":  "text/csv",
	".json": "application/json",
}

// TestGolden is the story's headline acceptance test: every fixture under testdata/
// parses to a stable Normalised representation captured in testdata/golden/. Run
// with -update to (re)generate goldens, then eyeball them.
func TestGolden(t *testing.T) {
	reg := Default()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		mime, ok := extMIME[strings.ToLower(filepath.Ext(name))]
		if !ok {
			continue
		}
		fixtures++
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			got, err := reg.Parse(mime, data)
			if err != nil {
				t.Fatalf("Parse(%s): %v", mime, err)
			}
			pretty, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			pretty = append(pretty, '\n')
			goldenPath := filepath.Join("testdata", "golden", name+".json")
			if *update {
				if err := os.WriteFile(goldenPath, pretty, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run -update first): %v", err)
			}
			if string(want) != string(pretty) {
				t.Errorf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", name, pretty, want)
			}
		})
	}
	if fixtures < 10 {
		t.Fatalf("expected >=10 representative fixtures, found %d", fixtures)
	}
}

func TestHTMLBoilerplateRemoved(t *testing.T) {
	data := mustRead(t, "article.html")
	n, err := Default().Parse("text/html", data)
	if err != nil {
		t.Fatal(err)
	}
	md := n.Markdown()
	// Site chrome must be gone.
	for _, boiler := range []string{"Skip to content", "Main navigation", "Cookie", "All rights reserved", "Advertisement"} {
		if strings.Contains(md, boiler) {
			t.Errorf("boilerplate %q leaked into output:\n%s", boiler, md)
		}
	}
	// Real article content must survive.
	if !strings.Contains(md, "Why Retrieval Augmented Generation Works") {
		t.Errorf("article heading missing from output:\n%s", md)
	}
	if !strings.Contains(md, "grounding") {
		t.Errorf("article body missing from output:\n%s", md)
	}
	if n.Title == "" {
		t.Error("expected a title extracted from the page")
	}
}

func TestHTMLHeadingsPreserved(t *testing.T) {
	n, err := Default().Parse("text/html", mustRead(t, "nested_headings.html"))
	if err != nil {
		t.Fatal(err)
	}
	var levels []int
	for _, b := range n.Blocks {
		if b.Type == Heading {
			levels = append(levels, b.Level)
		}
	}
	want := []int{1, 2, 3, 2}
	if len(levels) != len(want) {
		t.Fatalf("heading levels = %v, want %v", levels, want)
	}
	for i := range want {
		if levels[i] != want[i] {
			t.Fatalf("heading levels = %v, want %v", levels, want)
		}
	}
}

func TestHTMLTablesToMarkdown(t *testing.T) {
	n, err := Default().Parse("text/html", mustRead(t, "tables.html"))
	if err != nil {
		t.Fatal(err)
	}
	var tbl *Block
	for i := range n.Blocks {
		if n.Blocks[i].Type == Table {
			tbl = &n.Blocks[i]
			break
		}
	}
	if tbl == nil {
		t.Fatal("no table block emitted")
	}
	if len(tbl.Rows) < 2 {
		t.Fatalf("table rows = %d, want >=2", len(tbl.Rows))
	}
	if !strings.Contains(tbl.Text, "| --- |") && !strings.Contains(tbl.Text, "---") {
		t.Errorf("table markdown missing separator row:\n%s", tbl.Text)
	}
	if !strings.HasPrefix(tbl.Text, "|") {
		t.Errorf("table markdown should start with a pipe:\n%s", tbl.Text)
	}
}

func TestMarkdownHeadingsAndTitle(t *testing.T) {
	n, err := Default().Parse("text/markdown", mustRead(t, "guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Title != "Deployment Guide" {
		t.Errorf("title = %q, want %q", n.Title, "Deployment Guide")
	}
	if n.Blocks[0].Type != Heading || n.Blocks[0].Level != 1 {
		t.Errorf("first block = %+v, want H1", n.Blocks[0])
	}
	var hasCode bool
	for _, b := range n.Blocks {
		if b.Type == Code {
			hasCode = true
		}
	}
	if !hasCode {
		t.Error("fenced code block not detected")
	}
}

func TestCSVToMarkdownTable(t *testing.T) {
	n, err := Default().Parse("text/csv", mustRead(t, "employees.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Blocks) != 1 || n.Blocks[0].Type != Table {
		t.Fatalf("blocks = %+v, want single table", n.Blocks)
	}
	tbl := n.Blocks[0]
	if got := tbl.Rows[0]; strings.Join(got, ",") != "name,team,role" {
		t.Errorf("header row = %v", got)
	}
	if !strings.Contains(tbl.Text, "| name | team | role |") {
		t.Errorf("markdown header missing:\n%s", tbl.Text)
	}
}

func TestJSONRecordsToTable(t *testing.T) {
	n, err := Default().Parse("application/json", mustRead(t, "records.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Blocks) != 1 || n.Blocks[0].Type != Table {
		t.Fatalf("blocks = %+v, want single table", n.Blocks)
	}
	// Columns are the sorted union of keys for determinism.
	if got := strings.Join(n.Blocks[0].Rows[0], ","); got != "active,id,name" {
		t.Errorf("columns = %q, want active,id,name", got)
	}
}

func TestJSONNestedToKeyValue(t *testing.T) {
	n, err := Default().Parse("application/json", mustRead(t, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Blocks) != 1 || n.Blocks[0].Type != Code {
		t.Fatalf("blocks = %+v, want single code block", n.Blocks)
	}
	text := n.Blocks[0].Text
	if !strings.Contains(text, "database.host: db.internal") {
		t.Errorf("flattened key path missing:\n%s", text)
	}
	if !strings.Contains(text, "features[0]: search") {
		t.Errorf("flattened array index missing:\n%s", text)
	}
}

func TestTextParagraphs(t *testing.T) {
	n, err := Default().Parse("text/plain", mustRead(t, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var paras int
	for _, b := range n.Blocks {
		if b.Type == Paragraph {
			paras++
		}
	}
	if paras != 3 {
		t.Fatalf("paragraphs = %d, want 3", paras)
	}
}

func TestRegistryUnsupportedMIME(t *testing.T) {
	_, err := Default().Parse("application/zip", []byte("x"))
	if !errors.Is(err, ErrUnsupportedMIME) {
		t.Fatalf("err = %v, want ErrUnsupportedMIME", err)
	}
}

func TestRegistryStripsMIMEParams(t *testing.T) {
	// A content type with parameters still resolves to the base parser.
	_, err := Default().Parse("text/html; charset=utf-8", []byte("<p>hi</p>"))
	if err != nil {
		t.Fatalf("charset param should be stripped: %v", err)
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
