package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/rag-platform/ragctl/internal/connector"
)

// STORY-07.7 (FR-SRC-07/08, SPEC-04 §4, ADR-0049) — the per-item MAPPING that
// replaces 07.6's placeholder buildDocument. Each item's decoded JSON is the dot
// context for two text/template renders (body + uri_template); metadata is
// extracted with the 07.6 dot-path evaluator (jsonpath.go); updated_path becomes
// ModifiedAt and the incremental cursor value.

// templateFuncs are the helper functions available in both the body template and
// uri_template (SPEC-04 §4: "helpers join, money, date"). They are total — a helper
// never returns an error — so a helper can never abort a render; predictable
// fallbacks (verbatim value, empty string) handle malformed input instead. Errors
// therefore only ever come from field navigation (e.g. descending into a scalar),
// which name author-supplied template paths and Go types, never item values.
var templateFuncs = template.FuncMap{
	"join":  tmplJoin,
	"money": tmplMoney,
	"date":  tmplDate,
}

// docMapper renders one endpoint's items into connector.Documents. Its templates are
// parsed ONCE (in newDocMapper) and reused for every item across every page (SPEC-04
// §4 AC: "compile each endpoint's template once").
type docMapper struct {
	name        string
	idPath      string
	updatedPath string
	metadata    map[string]string
	body        *template.Template // nil when no template configured (raw-JSON fallback)
	uri         *template.Template // nil when no uri_template configured
}

// mapped is one item's mapping result: the Document plus the source updated_at value
// (verbatim raw + parsed) the caller folds into the incremental cursor.
type mapped struct {
	doc         connector.Document
	updatedRaw  string
	updatedTime time.Time
	hasUpdated  bool
}

// newDocMapper compiles an endpoint's templates. A template PARSE error is a config
// problem affecting every item, so it is returned here (Sync fails loud) rather than
// per item; validateSemantics also parses them so a bad template is caught at
// ValidateConfig/Test time.
func newDocMapper(ep endpoint) (*docMapper, error) {
	m := &docMapper{
		name:        ep.Name,
		idPath:      ep.IDPath,
		updatedPath: ep.UpdatedPath,
		metadata:    ep.Metadata,
	}
	var err error
	if strings.TrimSpace(ep.Template) != "" {
		if m.body, err = template.New(ep.Name + "/body").Funcs(templateFuncs).Parse(ep.Template); err != nil {
			return nil, fmt.Errorf("api: endpoint %q template: %w", ep.Name, err)
		}
	}
	if strings.TrimSpace(ep.URITemplate) != "" {
		if m.uri, err = template.New(ep.Name + "/uri").Funcs(templateFuncs).Parse(ep.URITemplate); err != nil {
			return nil, fmt.Errorf("api: endpoint %q uri_template: %w", ep.Name, err)
		}
	}
	return m, nil
}

// build maps one decoded JSON item to a Document. A template EXECUTION error is
// returned (not fatal): Sync records it and skips the item so one bad record cannot
// abort the whole sync. The error carries only the template location and the
// author-supplied path, never the item's content (no secret leakage).
func (m *docMapper) build(item any, seq int) (mapped, error) {
	raw, _ := json.Marshal(item)

	id, ok := evalString(item, m.idPath)
	if !ok || id == "" {
		id = strconv.Itoa(seq)
	}

	doc := connector.Document{
		ExternalID: m.name + "/" + id,
		RawJSON:    json.RawMessage(raw),
	}

	if m.body != nil {
		var buf bytes.Buffer
		if err := m.body.Execute(&buf, item); err != nil {
			return mapped{}, fmt.Errorf("api: endpoint %q body render: %w", m.name, err)
		}
		doc.Text = buf.String()
		doc.MimeType = "text/markdown"
	} else {
		// No template configured: emit the raw record as the body (the 07.6 behaviour),
		// so an endpoint that only needs metadata/id still ingests something.
		doc.Text = string(raw)
		doc.MimeType = "application/json"
	}

	if m.uri != nil {
		var buf bytes.Buffer
		if err := m.uri.Execute(&buf, item); err != nil {
			return mapped{}, fmt.Errorf("api: endpoint %q uri render: %w", m.name, err)
		}
		doc.URI = buf.String()
	}

	if len(m.metadata) > 0 {
		md := make(map[string]any, len(m.metadata))
		for k, path := range m.metadata {
			if v, ok := evalPath(item, path); ok && v != nil {
				md[k] = v
			}
		}
		if len(md) > 0 {
			doc.Metadata = md
		}
	}

	res := mapped{doc: doc}
	if strings.TrimSpace(m.updatedPath) != "" {
		if s, ok := evalString(item, m.updatedPath); ok {
			if t, ok := parseTimeString(s); ok {
				tt := t
				doc.ModifiedAt = &tt
				res.doc = doc
				res.updatedRaw = s
				res.updatedTime = t
				res.hasUpdated = true
			}
		}
	}
	return res, nil
}

// --- template helpers -------------------------------------------------------

// tmplJoin joins a list with a separator: {{join .tags ", "}}. It accepts a decoded
// JSON array ([]any), a []string, or nil/absent (which joins to ""). Non-list input
// renders as its fmt value so the template never errors.
func tmplJoin(list any, sep string) string {
	switch v := list.(type) {
	case nil:
		return ""
	case []string:
		return strings.Join(v, sep)
	case []any:
		parts := make([]string, len(v))
		for i, e := range v {
			parts[i] = fmt.Sprint(e)
		}
		return strings.Join(parts, sep)
	default:
		return fmt.Sprint(list)
	}
}

// tmplMoney formats a numeric amount with two decimals, optionally suffixed with a
// currency: {{money .price .currency}} -> "19.50 USD", {{money .price}} -> "19.50".
// The amount may be a json.Number, a float/int, or a numeric string. A non-numeric
// amount renders verbatim (predictable, never an error). An empty currency is omitted.
func tmplMoney(amount any, currency ...string) string {
	var s string
	if f, ok := toFloat(amount); ok {
		s = strconv.FormatFloat(f, 'f', 2, 64)
	} else {
		s = fmt.Sprint(amount)
	}
	if len(currency) > 0 {
		if cur := strings.TrimSpace(currency[0]); cur != "" {
			return s + " " + cur
		}
	}
	return s
}

// dateInputLayouts are the timestamp forms updated_at / date() accept, tried in
// order. Zulu/offset RFC3339 first, then naive datetime, then date-only.
var dateInputLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"2006/01/02",
}

// tmplDate parses value and reformats it: {{date .updated_at "2006-01-02"}}. The
// output layout is a Go reference layout; it defaults to RFC3339 when omitted. value
// may be a timestamp string (see dateInputLayouts) or epoch seconds (a json.Number/
// number). An unparseable value renders verbatim (predictable, never an error).
func tmplDate(value any, layout ...string) string {
	out := time.RFC3339
	if len(layout) > 0 && strings.TrimSpace(layout[0]) != "" {
		out = layout[0]
	}
	if t, ok := parseTimeAny(value); ok {
		return t.Format(out)
	}
	return fmt.Sprint(value)
}

// --- shared parsing ---------------------------------------------------------

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// parseTimeAny parses a decoded-JSON value as a timestamp: a string via
// parseTimeString, or a number as Unix epoch seconds.
func parseTimeAny(value any) (time.Time, bool) {
	switch v := value.(type) {
	case string:
		return parseTimeString(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return time.Unix(i, 0).UTC(), true
		}
		return parseTimeString(v.String())
	case float64:
		return time.Unix(int64(v), 0).UTC(), true
	default:
		return time.Time{}, false
	}
}

// parseTimeString parses a timestamp string against the accepted layouts, falling
// back to epoch seconds. It returns ok=false for empty/unparseable input.
func parseTimeString(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range dateInputLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(i, 0).UTC(), true
	}
	return time.Time{}, false
}
