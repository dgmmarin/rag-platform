package eval

import (
	"context"
	"errors"
	"testing"
	"time"
)

func boolp(b bool) *bool { return &b }

func TestDistinct(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{}, nil},
		{[]string{"a"}, []string{"a"}},
		{[]string{"a", "a", "b", "a"}, []string{"a", "b"}}, // dedup, order preserved
		{[]string{"a", "", "b"}, []string{"a", "b"}},       // empties dropped
	}
	for _, c := range cases {
		got := distinct(c.in)
		if len(got) != len(c.want) {
			t.Errorf("distinct(%v) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("distinct(%v)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestRecallHit(t *testing.T) {
	// No expected docs → excluded from recall (nil, not a miss).
	if got := recallHit(nil, []string{"a"}); got != nil {
		t.Errorf("recallHit(nil expected) = %v, want nil", *got)
	}
	// At least one expected doc present in retrieved → hit.
	if got := recallHit([]string{"x", "y"}, []string{"z", "y"}); got == nil || !*got {
		t.Errorf("recallHit intersecting = %v, want true", got)
	}
	// No expected doc present → miss.
	if got := recallHit([]string{"x"}, []string{"z", "y"}); got == nil || *got {
		t.Errorf("recallHit disjoint = %v, want false", got)
	}
	// Expected present but retrieved empty (e.g. retrieval error) → miss.
	if got := recallHit([]string{"x"}, nil); got == nil || *got {
		t.Errorf("recallHit empty retrieved = %v, want false", got)
	}
}

func TestSummarize(t *testing.T) {
	results := []CaseResult{
		{RecallHit: boolp(true), Grounded: true, LatencyMs: 100},
		{RecallHit: boolp(false), Grounded: true, LatencyMs: 200},
		{RecallHit: nil, Grounded: false, LatencyMs: 300},                                  // no expected docs → excluded from recall
		{RecallHit: boolp(true), Grounded: false, LatencyMs: 400, Err: errors.New("boom")}, // errored case still counted
	}
	s := summarize(8, results)
	if s.Cases != 4 || s.K != 8 {
		t.Fatalf("cases/k = %d/%d", s.Cases, s.K)
	}
	// recall@k = hits / cases-with-expected = 2 / 3
	if s.CasesScoredForRecall != 3 {
		t.Errorf("CasesScoredForRecall = %d, want 3", s.CasesScoredForRecall)
	}
	if s.RecallAtK < 0.66 || s.RecallAtK > 0.67 {
		t.Errorf("RecallAtK = %v, want ~0.667", s.RecallAtK)
	}
	// grounded rate = 2 / 4
	if s.GroundedRate != 0.5 {
		t.Errorf("GroundedRate = %v, want 0.5", s.GroundedRate)
	}
	// mean latency = (100+200+300+400)/4 = 250
	if s.MeanLatencyMs != 250 {
		t.Errorf("MeanLatencyMs = %d, want 250", s.MeanLatencyMs)
	}
	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1", s.Errors)
	}
}

func TestSummarizeEmpty(t *testing.T) {
	s := summarize(5, nil)
	if s.Cases != 0 || s.RecallAtK != 0 || s.GroundedRate != 0 || s.MeanLatencyMs != 0 {
		t.Fatalf("empty summary = %+v", s)
	}
}

// --- Runner with fakes (no DB, no real pipeline). ---

type fakeCaseSource struct {
	cs  []Case
	err error
}

func (f fakeCaseSource) cases(context.Context) ([]Case, error) { return f.cs, f.err }

type fakeSink struct {
	config   map[string]any
	results  []CaseResult
	finished Summary
	runID    string
}

func (f *fakeSink) create(_ context.Context, config map[string]any) (string, error) {
	f.config = config
	f.runID = "run-1"
	return f.runID, nil
}
func (f *fakeSink) record(_ context.Context, runID string, r CaseResult) error {
	if runID != f.runID {
		return errors.New("wrong run id")
	}
	f.results = append(f.results, r)
	return nil
}
func (f *fakeSink) finish(_ context.Context, _ string, s Summary) error {
	f.finished = s
	return nil
}

type fakePipeline struct {
	docs        map[string][]string
	grounded    map[string]bool
	retrieveErr map[string]error
	answerErr   map[string]error
}

func (f fakePipeline) Retrieve(_ context.Context, q string, _ int) ([]string, error) {
	if err := f.retrieveErr[q]; err != nil {
		return nil, err
	}
	return f.docs[q], nil
}
func (f fakePipeline) Answer(_ context.Context, q string, _ int) (bool, string, error) {
	if err := f.answerErr[q]; err != nil {
		return false, "", err
	}
	return f.grounded[q], "answer to " + q, nil
}

func TestRunnerRunGoldenPath(t *testing.T) {
	cases := []Case{
		{ID: "c1", Question: "q1", ExpectedDocIDs: []string{"d1"}},
		{ID: "c2", Question: "q2", ExpectedDocIDs: []string{"d9"}}, // will miss
		{ID: "c3", Question: "q3"},                                 // no expected → excluded from recall
	}
	pipe := fakePipeline{
		docs:     map[string][]string{"q1": {"d1", "d1", "d2"}, "q2": {"d2"}, "q3": {"d3"}},
		grounded: map[string]bool{"q1": true, "q2": true, "q3": false},
	}
	sink := &fakeSink{}
	clock := newStepClock(0, 10*time.Millisecond)
	r := &Runner{
		Cases:    fakeCaseSource{cs: cases},
		Sink:     sink,
		Pipeline: pipe,
		K:        5,
		Now:      clock,
	}
	got, err := r.Run(context.Background(), map[string]any{"retrieval": map[string]any{"final_k": 5}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.results) != 3 {
		t.Fatalf("recorded %d results, want 3", len(sink.results))
	}
	// c1 stored distinct retrieved docs (dedup d1).
	if len(sink.results[0].RetrievedDocIDs) != 2 {
		t.Errorf("c1 retrieved = %v, want [d1 d2]", sink.results[0].RetrievedDocIDs)
	}
	if sink.results[0].RecallHit == nil || !*sink.results[0].RecallHit {
		t.Errorf("c1 should be a recall hit")
	}
	if sink.results[1].RecallHit == nil || *sink.results[1].RecallHit {
		t.Errorf("c2 should be a recall miss")
	}
	if sink.results[2].RecallHit != nil {
		t.Errorf("c3 (no expected) should have nil recall_hit")
	}
	// recall@k = 1 hit / 2 scored = 0.5; grounded = 2/3; latency = 10ms each.
	if got.RecallAtK != 0.5 || got.CasesScoredForRecall != 2 {
		t.Errorf("summary recall = %v over %d", got.RecallAtK, got.CasesScoredForRecall)
	}
	if got.MeanLatencyMs != 10 {
		t.Errorf("mean latency = %d, want 10", got.MeanLatencyMs)
	}
	if got.RunID != "run-1" {
		t.Errorf("RunID = %q", got.RunID)
	}
	if sink.finished.Cases != 3 {
		t.Errorf("finish summary cases = %d", sink.finished.Cases)
	}
}

func TestRunnerRunFailSoftPerCase(t *testing.T) {
	cases := []Case{
		{ID: "c1", Question: "ok", ExpectedDocIDs: []string{"d1"}},
		{ID: "c2", Question: "boom", ExpectedDocIDs: []string{"d1"}},
	}
	pipe := fakePipeline{
		docs:        map[string][]string{"ok": {"d1"}},
		grounded:    map[string]bool{"ok": true},
		retrieveErr: map[string]error{"boom": errors.New("retrieval down")},
		answerErr:   map[string]error{"boom": errors.New("answer down")},
	}
	sink := &fakeSink{}
	r := &Runner{Cases: fakeCaseSource{cs: cases}, Sink: sink, Pipeline: pipe, K: 3}
	got, err := r.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run should fail-soft per case, not abort: %v", err)
	}
	if len(sink.results) != 2 {
		t.Fatalf("recorded %d, want 2 (errored case still recorded)", len(sink.results))
	}
	if sink.results[1].Err == nil {
		t.Errorf("c2 should carry its pipeline error")
	}
	if sink.results[1].RecallHit == nil || *sink.results[1].RecallHit {
		t.Errorf("c2 errored → recall miss")
	}
	if got.Errors != 1 {
		t.Errorf("summary Errors = %d, want 1", got.Errors)
	}
}

func TestRunnerRunConfigStored(t *testing.T) {
	sink := &fakeSink{}
	r := &Runner{Cases: fakeCaseSource{}, Sink: sink, Pipeline: fakePipeline{}, K: 1}
	cfg := map[string]any{"retrieval": map[string]any{"final_k": 12}}
	if _, err := r.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.config == nil {
		t.Fatal("effective config should be passed to the run store")
	}
}

// newStepClock returns a Now func that advances by step on every call, starting
// at start, so per-case latency is deterministic in tests.
func newStepClock(start, step time.Duration) func() time.Time {
	base := time.Unix(0, 0).Add(start)
	n := 0
	return func() time.Time {
		t := base.Add(time.Duration(n) * step)
		n++
		return t
	}
}
