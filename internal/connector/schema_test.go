package connector

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// A minimal web-crawl-like schema: an object requiring a non-empty start_urls
// array and forbidding unknown properties. Used to prove ValidateConfig teeth.
const testSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["start_urls"],
  "properties": {
    "start_urls": {"type": "array", "minItems": 1, "items": {"type": "string"}},
    "max_depth": {"type": "integer", "minimum": 0}
  }
}`

func TestSchemaValidatorAcceptsValidConfig(t *testing.T) {
	v := MustSchemaValidator([]byte(testSchema))
	cfg := json.RawMessage(`{"start_urls":["https://docs.acme.com/"],"max_depth":3}`)
	if err := v.Validate(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestSchemaValidatorRejectsMissingRequired(t *testing.T) {
	v := MustSchemaValidator([]byte(testSchema))
	cfg := json.RawMessage(`{"max_depth":3}`)
	err := v.Validate(cfg)
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ConfigError, got %v", err)
	}
	if len(ce.Fields) == 0 {
		t.Fatal("ConfigError carried no field violations")
	}
}

func TestSchemaValidatorRejectsUnknownProperty(t *testing.T) {
	v := MustSchemaValidator([]byte(testSchema))
	cfg := json.RawMessage(`{"start_urls":["x"],"bogus":true}`)
	if err := v.Validate(cfg); err == nil {
		t.Fatal("unknown property accepted")
	}
}

func TestSchemaValidatorRejectsWrongType(t *testing.T) {
	v := MustSchemaValidator([]byte(testSchema))
	// start_urls must be an array, not a string; and empty violates minItems.
	for _, bad := range []string{`{"start_urls":"nope"}`, `{"start_urls":[]}`} {
		if err := v.Validate(json.RawMessage(bad)); err == nil {
			t.Fatalf("config %q accepted", bad)
		}
	}
}

func TestSchemaValidatorRejectsNonObjectAndInvalidJSON(t *testing.T) {
	v := MustSchemaValidator([]byte(testSchema))
	if err := v.Validate(json.RawMessage(`[1,2,3]`)); err == nil {
		t.Fatal("non-object config accepted")
	}
	if err := v.Validate(json.RawMessage(`{not json`)); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestConfigErrorMessageListsFields(t *testing.T) {
	ce := &ConfigError{Fields: []FieldError{{Field: "start_urls", Message: "is required"}}}
	if !strings.Contains(ce.Error(), "start_urls") {
		t.Fatalf("error string missing field: %q", ce.Error())
	}
}

func TestNewSchemaValidatorRejectsBadSchema(t *testing.T) {
	if _, err := NewSchemaValidator([]byte(`{not a schema`)); err == nil {
		t.Fatal("compiling a malformed schema should error")
	}
}
