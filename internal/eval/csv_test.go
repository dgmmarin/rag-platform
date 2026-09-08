package eval

import (
	"errors"
	"strings"
	"testing"
)

// ParseCSV is the trust boundary for CSV import (STORY-12.1, FR-ADM-04): it must
// reject malformed input rather than write half-validated cases. These tests pin
// the documented CSV contract (ADR-0069): a header row, `|`-separated lists, and
// an optional `id` column that turns a create into an upsert.

func TestParseCSVHappyPath(t *testing.T) {
	in := strings.Join([]string{
		"question,expected_answer,expected_doc_ids,tags",
		"What is our refund policy?,Refunds within 30 days.,11111111-1111-1111-1111-111111111111|22222222-2222-2222-2222-222222222222,billing|policy",
	}, "\n")

	rows, err := ParseCSV(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseCSV: unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Question != "What is our refund policy?" {
		t.Errorf("question = %q", r.Question)
	}
	if r.ExpectedAnswer == nil || *r.ExpectedAnswer != "Refunds within 30 days." {
		t.Errorf("expected_answer = %v", r.ExpectedAnswer)
	}
	if len(r.ExpectedDocIDs) != 2 || r.ExpectedDocIDs[0] != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("expected_doc_ids = %v", r.ExpectedDocIDs)
	}
	if len(r.Tags) != 2 || r.Tags[0] != "billing" || r.Tags[1] != "policy" {
		t.Errorf("tags = %v", r.Tags)
	}
	if r.ID != "" {
		t.Errorf("id should be empty (create), got %q", r.ID)
	}
}

func TestParseCSVQuestionOnly(t *testing.T) {
	// Only `question` is mandatory; the other columns may be absent entirely.
	rows, err := ParseCSV(strings.NewReader("question\nHow do I reset my password?\n"))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if len(rows) != 1 || rows[0].Question != "How do I reset my password?" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].ExpectedAnswer != nil {
		t.Errorf("expected_answer should be nil when column absent, got %v", rows[0].ExpectedAnswer)
	}
	if len(rows[0].ExpectedDocIDs) != 0 || len(rows[0].Tags) != 0 {
		t.Errorf("lists should be empty, got %+v", rows[0])
	}
}

func TestParseCSVUpsertIDColumn(t *testing.T) {
	in := "id,question\n33333333-3333-3333-3333-333333333333,Restated question\n"
	rows, err := ParseCSV(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if rows[0].ID != "33333333-3333-3333-3333-333333333333" {
		t.Fatalf("id = %q", rows[0].ID)
	}
}

func TestParseCSVMissingQuestionHeader(t *testing.T) {
	_, err := ParseCSV(strings.NewReader("expected_answer,tags\nfoo,bar\n"))
	if err == nil {
		t.Fatal("want error for missing required 'question' header")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "question") {
		t.Errorf("error should mention the missing column: %v", err)
	}
}

func TestParseCSVUnknownHeaderRejected(t *testing.T) {
	_, err := ParseCSV(strings.NewReader("question,frobnicate\nq,x\n"))
	if err == nil {
		t.Fatal("want error for unknown column header")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error should name the unknown column: %v", err)
	}
}

func TestParseCSVEmptyQuestionRejected(t *testing.T) {
	_, err := ParseCSV(strings.NewReader("question,tags\n   ,billing\n"))
	if err == nil {
		t.Fatal("want error for empty question")
	}
	if !strings.Contains(err.Error(), "row 2") {
		t.Errorf("error should name the offending row: %v", err)
	}
}

func TestParseCSVBadUUIDRejected(t *testing.T) {
	in := "question,expected_doc_ids\nq,not-a-uuid\n"
	_, err := ParseCSV(strings.NewReader(in))
	if err == nil {
		t.Fatal("want error for malformed expected_doc_ids UUID")
	}
	if !strings.Contains(err.Error(), "row 2") || !strings.Contains(err.Error(), "not-a-uuid") {
		t.Errorf("error should name row and bad value: %v", err)
	}
}

func TestParseCSVBadIDRejected(t *testing.T) {
	_, err := ParseCSV(strings.NewReader("id,question\nnope,q\n"))
	if err == nil {
		t.Fatal("want error for malformed id column value")
	}
}

func TestParseCSVEmptyInputRejected(t *testing.T) {
	_, err := ParseCSV(strings.NewReader(""))
	if err == nil {
		t.Fatal("want error for empty CSV (no header)")
	}
}

func TestParseCSVNoDataRows(t *testing.T) {
	// A header with no data rows is a valid, empty import (not an error).
	rows, err := ParseCSV(strings.NewReader("question,tags\n"))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("want 0 rows, got %d", len(rows))
	}
}

func TestParseCSVHeaderCaseInsensitiveAndTrimmed(t *testing.T) {
	rows, err := ParseCSV(strings.NewReader(" Question , Tags \nq,a|b\n"))
	if err != nil {
		t.Fatalf("ParseCSV: %v", err)
	}
	if rows[0].Question != "q" || len(rows[0].Tags) != 2 {
		t.Fatalf("row = %+v", rows[0])
	}
}
