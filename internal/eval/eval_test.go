package eval

import (
	"errors"
	"testing"
	"time"
)

func TestSplitList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a|b|c", []string{"a", "b", "c"}},
		{" a | b ", []string{"a", "b"}}, // trimmed
		{"a||b", []string{"a", "b"}},    // empty items dropped
		{"|a|", []string{"a"}},          // leading/trailing separators dropped
	}
	for _, c := range cases {
		got := splitList(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitList(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitList(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestCaseInputValidate(t *testing.T) {
	good := CaseInput{
		Question:       "Why is the sky blue?",
		ExpectedDocIDs: []string{"11111111-1111-1111-1111-111111111111"},
		Tags:           []string{"science"},
	}
	if err := good.validate(); err != nil {
		t.Fatalf("good input rejected: %v", err)
	}

	if err := (CaseInput{Question: "  "}).validate(); err == nil {
		t.Error("blank question should be rejected")
	}
	if err := (CaseInput{Question: "q", ExpectedDocIDs: []string{"bad"}}).validate(); err == nil {
		t.Error("malformed expected_doc_id should be rejected")
	}
	if err := (CaseInput{Question: "q", ID: "bad"}).validate(); err == nil {
		t.Error("malformed id should be rejected")
	}
}

// fakeRow implements the rowScanner seam so scanCase's column mapping can be
// exercised without a live tenant database (the real *tenant.DB is unforgeable,
// so the SQL round trip is covered by e2e; this pins the struct mapping).
type fakeRow struct {
	vals []any
	err  error
}

func (f fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = f.vals[i].(string)
		case **string:
			if f.vals[i] == nil {
				*d = nil
			} else {
				s := f.vals[i].(string)
				*d = &s
			}
		case *[]string:
			if f.vals[i] == nil {
				*d = nil
			} else {
				*d = f.vals[i].([]string)
			}
		case *time.Time:
			*d = f.vals[i].(time.Time)
		default:
			return errors.New("fakeRow: unsupported dest type")
		}
	}
	return nil
}

func TestScanCase(t *testing.T) {
	now := time.Now().UTC()
	ans := "an answer"
	row := fakeRow{vals: []any{
		"11111111-1111-1111-1111-111111111111", // id
		"a question",                           // question
		ans,                                    // expected_answer
		[]string{"aaaa"},                       // expected_doc_ids
		[]string{"t1", "t2"},                   // tags
		now,                                    // created_at
	}}
	c, err := scanCase(row)
	if err != nil {
		t.Fatalf("scanCase: %v", err)
	}
	if c.ID != "11111111-1111-1111-1111-111111111111" || c.Question != "a question" {
		t.Errorf("id/question wrong: %+v", c)
	}
	if c.ExpectedAnswer == nil || *c.ExpectedAnswer != ans {
		t.Errorf("expected_answer = %v", c.ExpectedAnswer)
	}
	if len(c.Tags) != 2 || !c.CreatedAt.Equal(now) {
		t.Errorf("tags/created_at wrong: %+v", c)
	}
}

func TestScanCaseNullAnswer(t *testing.T) {
	row := fakeRow{vals: []any{
		"11111111-1111-1111-1111-111111111111",
		"q",
		nil, // expected_answer NULL
		nil, // expected_doc_ids empty
		nil, // tags empty
		time.Now(),
	}}
	c, err := scanCase(row)
	if err != nil {
		t.Fatalf("scanCase: %v", err)
	}
	if c.ExpectedAnswer != nil {
		t.Errorf("expected_answer should be nil, got %v", *c.ExpectedAnswer)
	}
}
