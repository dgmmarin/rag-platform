package eval

import (
	"errors"
	"testing"
)

// reportRow is a fake rowScanner for scanReportResult: it assigns preset values
// into the scan destinations by type (the types scanReportResult uses).
type reportRow struct {
	vals []any
	err  error
}

func (r reportRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			*d = r.vals[i].(string)
		case **string:
			if r.vals[i] == nil {
				*d = nil
			} else {
				s := r.vals[i].(string)
				*d = &s
			}
		case *[]string:
			if r.vals[i] == nil {
				*d = nil
			} else {
				*d = r.vals[i].([]string)
			}
		case **bool:
			if r.vals[i] == nil {
				*d = nil
			} else {
				b := r.vals[i].(bool)
				*d = &b
			}
		case *int:
			*d = r.vals[i].(int)
		default:
			return errors.New("reportRow: unsupported dest type")
		}
	}
	return nil
}

func TestScanReportResult(t *testing.T) {
	hit := true
	row := reportRow{vals: []any{
		"11111111-1111-1111-1111-111111111111", // case_id
		"the question",                         // question (nullable)
		"the expected",                         // expected_answer (nullable)
		[]string{"d1", "d2"},                   // retrieved_doc_ids
		hit,                                    // recall_hit (nullable)
		nil,                                    // judged_correct (nullable → NULL)
		"the answer",                           // answer (nullable)
		42,                                     // latency_ms
	}}
	rv, err := scanReportResult(row)
	if err != nil {
		t.Fatalf("scanReportResult: %v", err)
	}
	if rv.CaseID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("case_id = %q", rv.CaseID)
	}
	if rv.Question == nil || *rv.Question != "the question" {
		t.Errorf("question = %v", rv.Question)
	}
	if len(rv.RetrievedDocIDs) != 2 {
		t.Errorf("retrieved = %v", rv.RetrievedDocIDs)
	}
	if rv.RecallHit == nil || !*rv.RecallHit {
		t.Errorf("recall_hit = %v", rv.RecallHit)
	}
	if rv.JudgedCorrect != nil {
		t.Errorf("judged_correct should be nil (NULL), got %v", *rv.JudgedCorrect)
	}
	if rv.LatencyMs != 42 {
		t.Errorf("latency = %d", rv.LatencyMs)
	}
}

func TestScanReportResultNullableFields(t *testing.T) {
	row := reportRow{vals: []any{
		"11111111-1111-1111-1111-111111111111",
		nil, // question NULL (case deleted since the run)
		nil, // expected_answer NULL
		nil, // retrieved_doc_ids empty
		nil, // recall_hit NULL
		nil, // judged_correct NULL
		nil, // answer NULL
		0,
	}}
	rv, err := scanReportResult(row)
	if err != nil {
		t.Fatalf("scanReportResult: %v", err)
	}
	if rv.Question != nil || rv.ExpectedAnswer != nil || rv.RecallHit != nil || rv.Answer != nil {
		t.Errorf("nullable fields should be nil: %+v", rv)
	}
}
