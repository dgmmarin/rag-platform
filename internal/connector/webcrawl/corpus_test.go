package webcrawl

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden regenerates the committed expected/*.md extraction baselines:
//
//	go test ./internal/connector/webcrawl/ -run TestGoldenCorpus -update
var updateGolden = flag.Bool("update", false, "regenerate golden corpus expected markdown")

// corpusPage mirrors one manifest.json entry (see the gencorpus generator).
type corpusPage struct {
	Name        string   `json:"name"`
	Include     []string `json:"include"`
	Exclude     []string `json:"exclude"`
	Content     []string `json:"content"`     // markers that MUST survive extraction
	Boilerplate []string `json:"boilerplate"` // markers that MUST be removed
}

const corpusDir = "testdata/corpus"

// TestGoldenCorpus is the STORY-07.3 acceptance/regression test (FR-SRC-05). It
// runs the extractor over a 20-page synthetic-but-representative corpus and asserts:
//
//   - CONTENT RETENTION == 1.0 per page — every known article marker survives, so a
//     high removal score can never be earned by dropping content (the degenerate
//     "extract nothing" cheat is caught here).
//   - MEAN BOILERPLATE REMOVAL >= 0.90 across the corpus — the fraction of known
//     chrome markers absent from the output, averaged over the pages.
//
// It also pins each page's extracted markdown against a committed golden baseline so
// a future extraction regression is caught deterministically. Hermetic: no network,
// no DB, no clock. Regenerate goldens with -update after a deliberate change.
func TestGoldenCorpus(t *testing.T) {
	pages := loadManifest(t)
	if len(pages) != 20 {
		t.Fatalf("corpus has %d pages, want 20", len(pages))
	}

	var totalRemoval float64
	for _, p := range pages {
		html, err := os.ReadFile(filepath.Join(corpusDir, p.Name+".html"))
		if err != nil {
			t.Fatalf("read %s: %v", p.Name, err)
		}
		out := extractContent(html, p.Include, p.Exclude)

		goldenPath := filepath.Join(corpusDir, "expected", p.Name+".md")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath, []byte(out), 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s (run -update to seed): %v", p.Name, err)
			}
			if string(want) != out {
				t.Errorf("%s: extracted markdown drifted from golden baseline\n--- got ---\n%s", p.Name, out)
			}
		}

		// Content retention: every article marker must survive.
		keptContent := countPresent(out, p.Content)
		if keptContent != len(p.Content) {
			t.Errorf("%s: content retention %d/%d — article markers dropped:\n%s",
				p.Name, keptContent, len(p.Content), missing(out, p.Content))
		}

		// Boilerplate removal for this page.
		removedBoiler := len(p.Boilerplate) - countPresent(out, p.Boilerplate)
		var removal float64 = 1
		if len(p.Boilerplate) > 0 {
			removal = float64(removedBoiler) / float64(len(p.Boilerplate))
		}
		totalRemoval += removal
		t.Logf("%-22s removal=%.2f (%d/%d chrome markers removed), content kept=%d/%d",
			p.Name, removal, removedBoiler, len(p.Boilerplate), keptContent, len(p.Content))
	}

	mean := totalRemoval / float64(len(pages))
	t.Logf("MEAN boilerplate removal across corpus = %.4f (threshold 0.90)", mean)
	if mean < 0.90 {
		t.Fatalf("mean boilerplate removal %.4f < 0.90 acceptance threshold (FR-SRC-05)", mean)
	}
}

func loadManifest(t *testing.T) []corpusPage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpusDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var pages []corpusPage
	if err := json.Unmarshal(raw, &pages); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return pages
}

func countPresent(haystack string, markers []string) int {
	n := 0
	for _, m := range markers {
		if strings.Contains(haystack, m) {
			n++
		}
	}
	return n
}

func missing(haystack string, markers []string) string {
	var out []string
	for _, m := range markers {
		if !strings.Contains(haystack, m) {
			out = append(out, m)
		}
	}
	return strings.Join(out, ", ")
}
