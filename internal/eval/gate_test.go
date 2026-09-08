package eval

import (
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

func TestParseGatePolicy(t *testing.T) {
	p, err := ParseGatePolicy([]byte(`{"min_recall_at_k":0.7,"min_grounded_rate":0.8}`))
	if err != nil {
		t.Fatalf("ParseGatePolicy: %v", err)
	}
	if p.MinRecallAtK == nil || *p.MinRecallAtK != 0.7 {
		t.Errorf("min_recall_at_k = %v", p.MinRecallAtK)
	}
	if p.MinGroundedRate == nil || *p.MinGroundedRate != 0.8 {
		t.Errorf("min_grounded_rate = %v", p.MinGroundedRate)
	}
	if p.MinCorrectnessRate != nil {
		t.Errorf("min_correctness_rate should be unset, got %v", *p.MinCorrectnessRate)
	}
}

func TestParseGatePolicyRejectsEmpty(t *testing.T) {
	// A policy with no thresholds is a no-op gate — reject it so a misconfigured
	// gate fails loudly rather than passing everything silently.
	if _, err := ParseGatePolicy([]byte(`{}`)); err == nil {
		t.Fatal("want error for a threshold-less policy")
	}
	if _, err := ParseGatePolicy([]byte(`not json`)); err == nil {
		t.Fatal("want error for invalid JSON")
	}
}

func TestCheckGatePass(t *testing.T) {
	s := Summary{Cases: 10, RecallAtK: 0.9, CasesScoredForRecall: 10, GroundedRate: 0.95}
	r := CheckGate(s, GatePolicy{MinRecallAtK: f64(0.7), MinGroundedRate: f64(0.8)})
	if !r.Passed || r.Skipped || len(r.Failures) != 0 {
		t.Fatalf("expected pass, got %+v", r)
	}
}

func TestCheckGateFailsBelowThreshold(t *testing.T) {
	s := Summary{Cases: 10, RecallAtK: 0.5, CasesScoredForRecall: 10, GroundedRate: 0.6}
	r := CheckGate(s, GatePolicy{MinRecallAtK: f64(0.7), MinGroundedRate: f64(0.8)})
	if r.Passed || r.Skipped {
		t.Fatalf("expected fail, got %+v", r)
	}
	if len(r.Failures) != 2 {
		t.Fatalf("expected 2 failures, got %v", r.Failures)
	}
	joined := strings.Join(r.Failures, " ")
	if !strings.Contains(joined, "recall") || !strings.Contains(joined, "grounded") {
		t.Errorf("failures should name the failing metrics: %v", r.Failures)
	}
}

func TestCheckGateSkipsOnNoCases(t *testing.T) {
	r := CheckGate(Summary{Cases: 0}, GatePolicy{MinRecallAtK: f64(0.7)})
	if !r.Skipped || r.Passed {
		t.Fatalf("expected skip on zero cases, got %+v", r)
	}
	if r.Reason == "" {
		t.Error("skip should carry a reason")
	}
}

func TestCheckGateCorrectness(t *testing.T) {
	// Correctness threshold set but no cases judged → fail (the gate expected a
	// --judge run), never a silent pass.
	r := CheckGate(Summary{Cases: 5, RecallAtK: 1, GroundedRate: 1, CasesJudged: 0},
		GatePolicy{MinCorrectnessRate: f64(0.6)})
	if r.Passed {
		t.Fatalf("expected fail when correctness threshold set but nothing judged, got %+v", r)
	}

	// Judged and below threshold → fail.
	r = CheckGate(Summary{Cases: 5, RecallAtK: 1, GroundedRate: 1, CasesJudged: 5, CorrectnessRate: 0.4},
		GatePolicy{MinCorrectnessRate: f64(0.6)})
	if r.Passed {
		t.Fatalf("expected fail below correctness threshold, got %+v", r)
	}

	// Judged and at/above threshold → pass.
	r = CheckGate(Summary{Cases: 5, RecallAtK: 1, GroundedRate: 1, CasesJudged: 5, CorrectnessRate: 0.8},
		GatePolicy{MinCorrectnessRate: f64(0.6)})
	if !r.Passed {
		t.Fatalf("expected pass at/above correctness threshold, got %+v", r)
	}
}

func TestCheckGateBoundaryIsInclusive(t *testing.T) {
	// Exactly at the threshold passes (>= not >).
	r := CheckGate(Summary{Cases: 3, RecallAtK: 0.7, CasesScoredForRecall: 3, GroundedRate: 0.8}, GatePolicy{MinRecallAtK: f64(0.7), MinGroundedRate: f64(0.8)})
	if !r.Passed {
		t.Fatalf("value exactly at threshold should pass, got %+v", r)
	}
}

func TestCheckGateRecallWithNoExpectedDocs(t *testing.T) {
	// A recall threshold with no cases carrying expected_doc_ids can't be measured;
	// the gate fails with a clear reason rather than reading the unmeasured 0.0 as a
	// below-threshold recall (mirrors the correctness "nothing judged" handling).
	r := CheckGate(Summary{Cases: 5, RecallAtK: 0, CasesScoredForRecall: 0, GroundedRate: 1},
		GatePolicy{MinRecallAtK: f64(0.7)})
	if r.Passed || r.Skipped {
		t.Fatalf("expected fail when recall threshold set but no expected_doc_ids, got %+v", r)
	}
	if len(r.Failures) != 1 || !strings.Contains(r.Failures[0], "expected_doc_ids") {
		t.Fatalf("failure should name the missing ground truth, got %v", r.Failures)
	}
}
