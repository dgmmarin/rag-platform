package eval

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
)

// csvColumns is the recognised header set (ADR-0069). `question` is required;
// the rest are optional. `id` present and non-empty makes a row an upsert.
var csvColumns = map[string]bool{
	"id":               true,
	"question":         true,
	"expected_answer":  true,
	"expected_doc_ids": true,
	"tags":             true,
}

// ParseCSV reads and fully validates a CSV import (STORY-12.1 trust boundary,
// FR-ADM-04). It fails closed on the FIRST problem — a missing/unknown header or
// any malformed row — returning a *ValidationError that names the offending
// column, row (1-based, header = row 1) and value, so an import never writes a
// half-validated set. A header with no data rows is a valid empty import.
//
// The lists (expected_doc_ids, tags) are `|`-separated within their cell; the
// cell itself is quoted per RFC 4180 by encoding/csv when it contains commas,
// quotes or newlines (ADR-0069).
func ParseCSV(r io.Reader) ([]CaseInput, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate ragged rows; we map by header, not position
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, invalid("empty CSV: a header row with a 'question' column is required")
	}
	if err != nil {
		return nil, invalid("reading CSV header: %v", err)
	}

	index, err := headerIndex(header)
	if err != nil {
		return nil, err
	}

	var out []CaseInput
	line := 1 // header consumed
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			return nil, invalid("row %d: %v", line, err)
		}
		in, err := rowToInput(index, rec, line)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, nil
}

// headerIndex normalises header names (trim + lower-case) and maps each to its
// column position, rejecting unknown columns and a missing required `question`.
func headerIndex(header []string) (map[string]int, error) {
	index := make(map[string]int, len(header))
	for i, raw := range header {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if !csvColumns[name] {
			return nil, invalid("unknown CSV column %q (allowed: id, question, expected_answer, expected_doc_ids, tags)", name)
		}
		index[name] = i
	}
	if _, ok := index["question"]; !ok {
		return nil, invalid("CSV is missing the required 'question' column")
	}
	return index, nil
}

// rowToInput maps one record to a validated CaseInput. line is the 1-based CSV
// line number for error messages.
func rowToInput(index map[string]int, rec []string, line int) (CaseInput, error) {
	get := func(col string) string {
		i, ok := index[col]
		if !ok || i >= len(rec) {
			return ""
		}
		return rec[i]
	}

	in := CaseInput{
		ID:             strings.TrimSpace(get("id")),
		Question:       strings.TrimSpace(get("question")),
		ExpectedDocIDs: splitList(get("expected_doc_ids")),
		Tags:           splitList(get("tags")),
	}
	if _, ok := index["expected_answer"]; ok {
		if ans := strings.TrimSpace(get("expected_answer")); ans != "" {
			in.ExpectedAnswer = &ans
		}
	}

	if err := in.validate(); err != nil {
		return CaseInput{}, rowError(line, err)
	}
	return in, nil
}

// rowError prefixes a validation error with the offending row number.
func rowError(line int, err error) error {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return invalid("row %d: %s", line, ve.Msg)
	}
	return fmt.Errorf("row %d: %w", line, err)
}
