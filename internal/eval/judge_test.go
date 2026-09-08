package eval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/llm"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"CORRECT", true, false},
		{"INCORRECT", false, false},
		{"correct", true, false},
		{"incorrect", false, false},
		{"Verdict: CORRECT.", true, false},
		{"The answer is INCORRECT because it omits the refund window.", false, false},
		{"  CORRECT\n", true, false},
		{"", false, true},            // empty → error, never a silent pass
		{"maybe", false, true},       // no verdict word → error
		{"CORRECTNESS", false, true}, // not a standalone verdict → error
	}
	for _, c := range cases {
		got, err := parseVerdict(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseVerdict(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseVerdict(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseVerdict(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBuildJudgePrompt(t *testing.T) {
	p := buildJudgePrompt("What is the refund window?", "30 days.", "You can get a refund within 30 days.")
	for _, want := range []string{"What is the refund window?", "30 days.", "You can get a refund within 30 days."} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// The three inputs must be delimited so they are treated as data, not
	// instructions (SPEC-09 §2 prompt-injection defence).
	if !strings.Contains(p, "QUESTION") || !strings.Contains(p, "EXPECTED") || !strings.Contains(p, "ACTUAL") {
		t.Errorf("prompt should label the three sections:\n%s", p)
	}
}

// fakeProvider is a fake llm.Provider for the judge tests (no network).
type fakeProvider struct {
	text   string
	err    error
	gotReq llm.Request
}

func (f *fakeProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.gotReq = req
	if f.err != nil {
		return llm.Response{}, f.err
	}
	return llm.Response{Text: f.text}, nil
}
func (f *fakeProvider) Stream(context.Context, llm.Request) (llm.Stream, error) {
	return nil, errors.New("unused")
}

func TestLLMJudgeVerdict(t *testing.T) {
	fp := &fakeProvider{text: "CORRECT"}
	j := NewLLMJudge(fp, "claude-judge")
	got, err := j.Judge(context.Background(), "q", "expected", "actual")
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if !got {
		t.Errorf("Judge = false, want true")
	}
	// The configured judge model and a low temperature (deterministic) must be set.
	if fp.gotReq.Model != "claude-judge" {
		t.Errorf("request model = %q, want claude-judge", fp.gotReq.Model)
	}
	if fp.gotReq.MaxTokens <= 0 {
		t.Errorf("request MaxTokens = %d, want > 0", fp.gotReq.MaxTokens)
	}
}

func TestLLMJudgeUnparseableIsError(t *testing.T) {
	j := NewLLMJudge(&fakeProvider{text: "I'm not sure about this one"}, "m")
	if _, err := j.Judge(context.Background(), "q", "e", "a"); err == nil {
		t.Fatal("an unparseable verdict must be an error (never a silent pass)")
	}
}

func TestLLMJudgeProviderErrorPropagates(t *testing.T) {
	j := NewLLMJudge(&fakeProvider{err: errors.New("provider down")}, "m")
	if _, err := j.Judge(context.Background(), "q", "e", "a"); err == nil {
		t.Fatal("a provider error must propagate (case → NULL, fail-soft)")
	}
}
