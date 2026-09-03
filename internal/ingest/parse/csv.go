package parse

import (
	"bytes"
	"encoding/csv"
	"io"
)

// csvParser handles text/csv via the stdlib reader and renders every row as one
// markdown table (SPEC-05 §2 "row/record → markdown table"). The first row is the
// header. Variable field counts are tolerated (FieldsPerRecord = -1).
type csvParser struct{}

func (csvParser) Parse(data []byte) (Normalised, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	var rows [][]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Normalised{}, err
		}
		rows = append(rows, rec)
	}
	if len(rows) == 0 {
		return Normalised{}, nil
	}
	return Normalised{
		Blocks: []Block{{Type: Table, Rows: rows, Text: markdownTable(rows)}},
	}, nil
}
