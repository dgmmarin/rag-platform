package connector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// errPrinter renders jsonschema ErrorKind messages in English; the library's
// exported helpers require a non-nil *message.Printer.
var errPrinter = message.NewPrinter(language.English)

// FieldError is one config-schema violation: the dotted path of the offending
// value ("start_urls", empty for the document root) and a human-readable message.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ConfigError is the aggregate of all field-level violations for one config
// document. Connectors return it from ValidateConfig; the sources API surfaces it
// as a 400 with the field list (SPEC-07 §1), mirroring tenant-settings validation
// (STORY-03.5). The message carries no secrets — config is non-credential data.
type ConfigError struct {
	Fields []FieldError
}

func (e *ConfigError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		if f.Field == "" {
			parts = append(parts, f.Message)
			continue
		}
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "invalid config: " + strings.Join(parts, "; ")
}

// SchemaValidator compiles a JSON Schema once and validates connector config
// against it (SPEC-04 §7 step 2). A connector embeds its schema JSON and builds
// one SchemaValidator at package init, then calls Validate from ValidateConfig —
// so config validation is declarative and identical across connectors (NFR-MNT-01).
type SchemaValidator struct {
	schema *jsonschema.Schema
}

// NewSchemaValidator compiles a JSON Schema document. It errors on a malformed or
// uncompilable schema so a connector can surface the problem loudly at init.
func NewSchemaValidator(schemaJSON []byte) (*SchemaValidator, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("connector: parse config schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	const url = "https://rag-platform/schemas/connector-config.json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("connector: add config schema resource: %w", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("connector: compile config schema: %w", err)
	}
	return &SchemaValidator{schema: sch}, nil
}

// MustSchemaValidator is NewSchemaValidator that panics on error, for use in a
// package-level var / init with an embedded (compile-time-known) schema.
func MustSchemaValidator(schemaJSON []byte) *SchemaValidator {
	v, err := NewSchemaValidator(schemaJSON)
	if err != nil {
		panic(err)
	}
	return v
}

// Validate checks a config document against the schema. It returns nil on success
// and a *ConfigError listing every leaf violation (sorted by field) otherwise.
// Malformed JSON and a non-object root are reported as ConfigErrors too.
func (v *SchemaValidator) Validate(cfg json.RawMessage) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(cfg))
	if err != nil {
		return &ConfigError{Fields: []FieldError{{Message: "config must be valid JSON"}}}
	}
	err = v.schema.Validate(inst)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return fmt.Errorf("connector: validate config: %w", err)
	}
	fields := collectLeafErrors(ve)
	if len(fields) == 0 {
		fields = []FieldError{{Field: instancePath(ve.InstanceLocation), Message: ve.ErrorKind.LocalizedString(errPrinter)}}
	}
	sort.Slice(fields, func(i, j int) bool {
		if fields[i].Field != fields[j].Field {
			return fields[i].Field < fields[j].Field
		}
		return fields[i].Message < fields[j].Message
	})
	return &ConfigError{Fields: fields}
}

// collectLeafErrors walks the ValidationError tree returning one FieldError per
// leaf (the most specific keyword that failed at an instance location).
func collectLeafErrors(ve *jsonschema.ValidationError) []FieldError {
	if len(ve.Causes) == 0 {
		return []FieldError{{
			Field:   instancePath(ve.InstanceLocation),
			Message: ve.ErrorKind.LocalizedString(errPrinter),
		}}
	}
	var out []FieldError
	for _, c := range ve.Causes {
		out = append(out, collectLeafErrors(c)...)
	}
	return out
}

// instancePath renders a JSON instance location as a dotted field path; the
// document root is the empty string.
func instancePath(loc []string) string {
	return strings.Join(loc, ".")
}
