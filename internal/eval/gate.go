package eval

import (
	"encoding/json"
	"fmt"
)

// This file is the STORY-12.4 CI-gate policy (FR-ADM-04, SPEC-06 §8): a pure
// comparison of a run's Summary against minimum-quality thresholds, so a
// settings/chunking/retrieval change must keep eval quality at or above the
// committed floor before it lands. The comparison is pure and unit-tested; the
// mise-tasks/eval-gate script and the CLI --gate flag orchestrate it. The
// baseline mechanism is deliberately "committed minimum thresholds" (a JSON file)
// rather than a stored prior-run baseline — simplest to reason about and to keep
// under version control (ADR-0072).

// GatePolicy is the set of minimum acceptable metrics. Each is optional (a nil
// threshold is not checked); at least one must be present (ParseGatePolicy
// enforces it) so a misconfigured empty policy never silently passes everything.
type GatePolicy struct {
	MinRecallAtK       *float64 `json:"min_recall_at_k,omitempty"`
	MinGroundedRate    *float64 `json:"min_grounded_rate,omitempty"`
	MinCorrectnessRate *float64 `json:"min_correctness_rate,omitempty"`
}

// GateResult is the outcome of a gate check. Skipped means the gate could not
// assess quality (no eval cases) — treated as a non-blocking pass by callers.
type GateResult struct {
	Passed   bool     `json:"passed"`
	Skipped  bool     `json:"skipped,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Failures []string `json:"failures,omitempty"`
}

// ParseGatePolicy parses a gate-policy JSON document and rejects a policy with no
// thresholds (a no-op gate is almost certainly a misconfiguration).
func ParseGatePolicy(data []byte) (GatePolicy, error) {
	var p GatePolicy
	if err := json.Unmarshal(data, &p); err != nil {
		return GatePolicy{}, fmt.Errorf("gate policy: %w", err)
	}
	if p.MinRecallAtK == nil && p.MinGroundedRate == nil && p.MinCorrectnessRate == nil {
		return GatePolicy{}, fmt.Errorf("gate policy: no thresholds set (need at least one of min_recall_at_k, min_grounded_rate, min_correctness_rate)")
	}
	return p, nil
}

// CheckGate compares a run summary against the policy. A run with no cases is
// Skipped (quality cannot be assessed). Otherwise every SET threshold is checked
// with a >= (at-threshold passes). A metric whose threshold is set but which
// cannot be measured (a recall threshold with no cases carrying expected_doc_ids,
// a correctness threshold with nothing judged) is a failure — the gate expected
// that ground truth — never a silent pass.
func CheckGate(s Summary, p GatePolicy) GateResult {
	if s.Cases == 0 {
		return GateResult{Skipped: true, Reason: "no eval cases to assess"}
	}
	var failures []string
	if p.MinRecallAtK != nil {
		switch {
		case s.CasesScoredForRecall == 0:
			failures = append(failures, "recall threshold set but no cases have expected_doc_ids")
		case s.RecallAtK < *p.MinRecallAtK:
			failures = append(failures, fmt.Sprintf("recall@k %.3f below minimum %.3f", s.RecallAtK, *p.MinRecallAtK))
		}
	}
	if p.MinGroundedRate != nil && s.GroundedRate < *p.MinGroundedRate {
		failures = append(failures, fmt.Sprintf("grounded rate %.3f below minimum %.3f", s.GroundedRate, *p.MinGroundedRate))
	}
	if p.MinCorrectnessRate != nil {
		switch {
		case s.CasesJudged == 0:
			failures = append(failures, "correctness threshold set but no cases were judged (run with --judge)")
		case s.CorrectnessRate < *p.MinCorrectnessRate:
			failures = append(failures, fmt.Sprintf("correctness rate %.3f below minimum %.3f", s.CorrectnessRate, *p.MinCorrectnessRate))
		}
	}
	return GateResult{Passed: len(failures) == 0, Failures: failures}
}
