package parse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// jsonParser handles application/json (SPEC-05 §2 "record → markdown table or
// key-value text"). A top-level array of objects becomes a markdown table (columns
// = sorted union of keys); anything else is flattened to stable "path: value"
// lines in a code block. Using json.Decoder with UseNumber keeps integers from
// being rendered in float/exponent form.
type jsonParser struct{}

func (jsonParser) Parse(data []byte) (Normalised, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Normalised{}, err
	}

	if rows, ok := recordsTable(v); ok {
		return Normalised{Blocks: []Block{{Type: Table, Rows: rows, Text: markdownTable(rows)}}}, nil
	}

	var lines []string
	flatten("", v, &lines)
	sort.Strings(lines)
	return Normalised{Blocks: []Block{{Type: Code, Text: strings.Join(lines, "\n")}}}, nil
}

// recordsTable renders a top-level array whose elements are all JSON objects as a
// table. Columns are the union of all keys, sorted for deterministic output. A
// cell holding a nested object/array is rendered as compact JSON.
func recordsTable(v any) ([][]string, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil, false
	}
	objs := make([]map[string]any, 0, len(arr))
	keySet := map[string]struct{}{}
	for _, el := range arr {
		m, ok := el.(map[string]any)
		if !ok {
			return nil, false
		}
		objs = append(objs, m)
		for k := range m {
			keySet[k] = struct{}{}
		}
	}
	cols := make([]string, 0, len(keySet))
	for k := range keySet {
		cols = append(cols, k)
	}
	sort.Strings(cols)

	rows := make([][]string, 0, len(objs)+1)
	rows = append(rows, cols)
	for _, m := range objs {
		row := make([]string, len(cols))
		for i, c := range cols {
			if val, ok := m[c]; ok {
				row[i] = cellString(val)
			}
		}
		rows = append(rows, row)
	}
	return rows, true
}

// cellString renders a value for a table cell: scalars plainly, composites as
// compact JSON (never multi-line, so it stays inside one table cell).
func cellString(v any) string {
	switch v.(type) {
	case map[string]any, []any:
		b, _ := json.Marshal(v)
		return string(b)
	default:
		return scalarString(v)
	}
}

// flatten walks arbitrary JSON into "dotted.path[i]: value" scalar lines.
func flatten(prefix string, v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			flatten(join(prefix, k), t[k], out)
		}
	case []any:
		for i, el := range t {
			flatten(fmt.Sprintf("%s[%d]", prefix, i), el, out)
		}
	default:
		key := prefix
		if key == "" {
			key = "value"
		}
		*out = append(*out, key+": "+scalarString(v))
	}
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// scalarString renders a JSON scalar deterministically. json.Number preserves the
// original numeric literal.
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(t)
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}
