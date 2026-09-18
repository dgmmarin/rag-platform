package obs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// knownMetrics is the SPEC-10 §2 catalogue this package registers (see metrics.go).
// The alert rules may only reference these — a typo or a renamed metric that would
// make an alert silently never fire is caught here at test time.
var knownMetrics = []string{
	"api_request_duration_seconds",
	"api_rate_limited_total",
	"query_retrieval_duration_seconds",
	"query_grounded_total",
	"ingest_documents_total",
	"ingest_chunks_total",
	"embed_tokens_total",
	"provider_request_duration_seconds",
	"provider_errors_total",
	"jobs_queue_depth",
	"jobs_duration_seconds",
	"jobs_failed_total",
	"tenant_pools_open",
	"tenant_schema_mismatch",
}

// ruleFile mirrors the Prometheus alerting-rules schema we commit (SPEC-10 §5).
type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// metricToken matches Prometheus metric identifiers so a reference is checked as a
// whole token, not a substring of a longer name.
var metricToken = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// TestAlertRulesParseAndReferenceRealMetrics is the STORY-10.2 runnable check
// (SPEC-10 §5): the committed Prometheus rules parse, and every alert is complete
// (name/expr/severity/summary + a runbook link) and references at least one metric
// this package actually registers — so an alert can never point at a metric that
// does not exist. No promtool dependency; a plain YAML parse against yaml.v3, which
// is already in the module graph.
func TestAlertRulesParseAndReferenceRealMetrics(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "prometheus", "rules", "ragctl.rules.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	var rf ruleFile
	if err := yaml.Unmarshal(raw, &rf); err != nil {
		t.Fatalf("rules do not parse as YAML: %v", err)
	}

	known := make(map[string]bool, len(knownMetrics))
	for _, m := range knownMetrics {
		known[m] = true
	}
	// A metric family exposes derived series (e.g. _bucket/_count/_sum for a
	// histogram); accept a token whose base (trimmed of those suffixes) is known.
	suffixes := []string{"_bucket", "_count", "_sum"}
	referencesKnown := func(expr string) (string, bool) {
		for _, tok := range metricToken.FindAllString(expr, -1) {
			base := tok
			for _, s := range suffixes {
				base = strings.TrimSuffix(base, s)
			}
			if known[base] {
				return tok, true
			}
		}
		return "", false
	}

	count := 0
	seen := map[string]bool{}
	for _, g := range rf.Groups {
		if g.Name == "" {
			t.Error("rule group missing a name")
		}
		for _, r := range g.Rules {
			count++
			if r.Alert == "" {
				t.Fatalf("group %q: an alert has no name", g.Name)
			}
			if seen[r.Alert] {
				t.Errorf("duplicate alert name %q", r.Alert)
			}
			seen[r.Alert] = true
			if strings.TrimSpace(r.Expr) == "" {
				t.Errorf("alert %q: empty expr", r.Alert)
			}
			if r.Labels["severity"] == "" {
				t.Errorf("alert %q: missing severity label", r.Alert)
			}
			if strings.TrimSpace(r.Annotations["summary"]) == "" {
				t.Errorf("alert %q: missing summary annotation", r.Alert)
			}
			// AC (STORY-10.2): a runbook link per alert.
			if !strings.HasPrefix(r.Annotations["runbook_url"], "http") {
				t.Errorf("alert %q: missing/invalid runbook_url annotation", r.Alert)
			}
			if _, ok := referencesKnown(r.Expr); !ok {
				t.Errorf("alert %q: expr references no known metric:\n%s", r.Alert, r.Expr)
			}
		}
	}
	if count == 0 {
		t.Fatal("no alert rules found")
	}
}
