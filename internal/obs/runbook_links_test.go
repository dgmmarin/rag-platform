package obs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestAlertRunbookLinksResolve is the STORY-10.8 runnable check: every alert's
// runbook_url in the committed Prometheus rules points at docs/runbooks/<file>#<anchor>,
// and that heading anchor must actually exist in that runbook — so an alert and its
// runbook cannot silently drift (the alert links a section that was renamed/removed).
// Plain file reads + yaml.v3 (already in the module graph); no new dependency.
func TestAlertRunbookLinksResolve(t *testing.T) {
	rulesPath := filepath.Join("..", "..", "deploy", "prometheus", "rules", "ragctl.rules.yml")
	raw, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	var rf ruleFile
	if err := yaml.Unmarshal(raw, &rf); err != nil {
		t.Fatalf("parse rules: %v", err)
	}

	// Cache runbook file -> set of heading anchors it defines.
	anchorsByFile := map[string]map[string]bool{}
	loadAnchors := func(file string) (map[string]bool, error) {
		if a, ok := anchorsByFile[file]; ok {
			return a, nil
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", "runbooks", file))
		if err != nil {
			return nil, err
		}
		a := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			if h := strings.TrimLeft(line, " "); strings.HasPrefix(h, "#") {
				a[slugify(strings.TrimSpace(strings.TrimLeft(h, "#")))] = true
			}
		}
		anchorsByFile[file] = a
		return a, nil
	}

	checked := 0
	for _, g := range rf.Groups {
		for _, r := range g.Rules {
			url := r.Annotations["runbook_url"]
			if url == "" {
				t.Errorf("alert %q: no runbook_url", r.Alert)
				continue
			}
			file, anchor, ok := runbookTarget(url)
			if !ok {
				t.Errorf("alert %q: runbook_url is not a docs/runbooks/<file>#<anchor> link: %s", r.Alert, url)
				continue
			}
			anchors, err := loadAnchors(file)
			if err != nil {
				t.Errorf("alert %q: runbook %q unreadable: %v", r.Alert, file, err)
				continue
			}
			if !anchors[anchor] {
				t.Errorf("alert %q: runbook_url anchor #%s has no matching heading in docs/runbooks/%s", r.Alert, anchor, file)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no alert runbook_url links were checked")
	}
}

// runbookTarget extracts the runbook filename and anchor from a runbook_url that
// points into docs/runbooks/ (the committed convention), tolerating either a full
// GitHub blob URL or a repo-relative path.
func runbookTarget(url string) (file, anchor string, ok bool) {
	const marker = "docs/runbooks/"
	i := strings.Index(url, marker)
	if i < 0 {
		return "", "", false
	}
	rest := url[i+len(marker):]
	hash := strings.Index(rest, "#")
	if hash < 0 {
		return "", "", false
	}
	return rest[:hash], rest[hash+1:], true
}

var nonSlug = regexp.MustCompile(`[^a-z0-9 -]`)

// slugify approximates GitHub's heading-anchor algorithm for the ASCII headings the
// runbooks use: lowercase, drop punctuation, spaces -> hyphens.
func slugify(h string) string {
	h = strings.ToLower(h)
	h = nonSlug.ReplaceAllString(h, "")
	return strings.ReplaceAll(strings.TrimSpace(h), " ", "-")
}
