package tenants

import (
	"bytes"
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// errPrinter renders ErrorKind messages in English; the library's exported
// helpers require a non-nil *message.Printer.
var errPrinter = message.NewPrinter(language.English)

// settingsSchemaJSON is the SPEC-02 §5 tenant-settings JSON Schema, embedded so
// validation needs no filesystem at runtime and the asset ships with the binary.
//
//go:embed settings_schema.json
var settingsSchemaJSON []byte

// compiledSchema is the parsed, compiled schema. Compiled once at package init so
// a malformed asset panics at startup (loud, once) instead of per request.
var compiledSchema = mustCompileSettingsSchema()

func mustCompileSettingsSchema() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(settingsSchemaJSON))
	if err != nil {
		panic(fmt.Sprintf("tenants: parse settings schema: %v", err))
	}
	c := jsonschema.NewCompiler()
	const url = "https://rag-platform/schemas/tenant-settings.json"
	if err := c.AddResource(url, doc); err != nil {
		panic(fmt.Sprintf("tenants: add settings schema resource: %v", err))
	}
	sch, err := c.Compile(url)
	if err != nil {
		panic(fmt.Sprintf("tenants: compile settings schema: %v", err))
	}
	return sch
}

// FieldError is one validation failure against the settings schema: the dotted
// path of the offending value (e.g. "embedding.dim", empty for the document
// root) and a human-readable message.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationErrors is the aggregate of all field-level failures for one write. It
// is returned by validateSettings and surfaced by the HTTP layer as a 400 with a
// per-field error list.
type ValidationErrors struct {
	Fields []FieldError
}

func (e *ValidationErrors) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		if f.Field == "" {
			parts = append(parts, f.Message)
			continue
		}
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "invalid settings: " + strings.Join(parts, "; ")
}

// validateSettings validates a decoded settings document against the embedded
// schema. On failure it returns *ValidationErrors carrying one FieldError per
// leaf violation, sorted by field for a stable response.
func validateSettings(doc any) error {
	err := compiledSchema.Validate(doc)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return fmt.Errorf("tenants: validate settings: %w", err)
	}
	fields := collectLeafErrors(ve)
	if len(fields) == 0 {
		// Defensive: a validation error with no leaf causes still means invalid.
		fields = []FieldError{{Field: instancePath(ve.InstanceLocation), Message: ve.ErrorKind.LocalizedString(errPrinter)}}
	}
	sort.Slice(fields, func(i, j int) bool {
		if fields[i].Field != fields[j].Field {
			return fields[i].Field < fields[j].Field
		}
		return fields[i].Message < fields[j].Message
	})
	return &ValidationErrors{Fields: fields}
}

// collectLeafErrors walks the ValidationError tree and returns one FieldError per
// leaf (a node with no further causes), which is the most specific keyword that
// actually failed at a given instance location.
func collectLeafErrors(ve *jsonschema.ValidationError) []FieldError {
	var out []FieldError
	if len(ve.Causes) == 0 {
		out = append(out, FieldError{
			Field:   instancePath(ve.InstanceLocation),
			Message: ve.ErrorKind.LocalizedString(errPrinter),
		})
		return out
	}
	for _, c := range ve.Causes {
		out = append(out, collectLeafErrors(c)...)
	}
	return out
}

// instancePath renders a JSON instance location as a dotted field path. The
// document root is the empty string.
func instancePath(loc []string) string {
	return strings.Join(loc, ".")
}
