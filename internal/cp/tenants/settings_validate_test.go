package tenants

import (
	"errors"
	"testing"
)

// The embedded JSON Schema must compile at package init so a broken asset fails
// loudly and once, not per request.
func TestSettingsSchemaCompiles(t *testing.T) {
	if compiledSchema == nil {
		t.Fatal("compiledSchema is nil; embedded schema failed to compile")
	}
}

// A document matching SPEC-02 §5 validates clean.
func TestValidateAcceptsSpecExample(t *testing.T) {
	doc := map[string]any{
		"embedding":         map[string]any{"provider": "voyage", "model": "voyage-3", "dim": 1024},
		"llm":               map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6", "max_tokens": 1024},
		"reranker":          map[string]any{"enabled": false, "provider": "cohere", "model": "rerank-v3.5", "top_n": 20},
		"chunking":          map[string]any{"target_tokens": 512, "overlap_tokens": 64},
		"retrieval":         map[string]any{"k_vector": 40, "k_text": 40, "final_k": 8, "min_score": 0.02},
		"limits":            map[string]any{"qps": 10, "max_upload_mb": 50, "max_pages_per_crawl": 5000},
		"providers_allowed": []any{"anthropic", "voyage", "cohere"},
	}
	if err := validateSettings(doc); err != nil {
		t.Fatalf("spec example rejected: %v", err)
	}
}

// An invalid document yields ErrInvalidSettings carrying per-field errors keyed
// by dotted instance path.
func TestValidateReturnsPerFieldErrors(t *testing.T) {
	doc := map[string]any{
		"embedding": map[string]any{"provider": "voyage", "model": "voyage-3", "dim": 0},        // dim below minimum
		"retrieval": map[string]any{"k_vector": 40, "k_text": 40, "final_k": 8, "min_score": 5}, // min_score > 1
		"bogus":     "nope",                                                                     // additionalProperties false
	}
	err := validateSettings(doc)
	if err == nil {
		t.Fatal("invalid document accepted; want ErrInvalidSettings")
	}
	var ve *ValidationErrors
	if !errors.As(err, &ve) {
		t.Fatalf("error type = %T, want *ValidationErrors", err)
	}
	fields := map[string]bool{}
	for _, fe := range ve.Fields {
		fields[fe.Field] = true
	}
	if !fields["embedding.dim"] {
		t.Errorf("missing field error for embedding.dim; got fields %v", fields)
	}
	if !fields["retrieval.min_score"] {
		t.Errorf("missing field error for retrieval.min_score; got fields %v", fields)
	}
	// A top-level unknown property is reported against the object root.
	found := false
	for _, fe := range ve.Fields {
		if fe.Field == "" || fe.Field == "bogus" {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown top-level property not reported; got fields %v", fields)
	}
}
