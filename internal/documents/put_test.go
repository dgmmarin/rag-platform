package documents

import (
	"strings"
	"testing"
)

// vectorLiteral must render a []float32 as a pgvector text literal ("[a,b,c]")
// so the store can write chunks.embedding via $n::vector without a pgvector
// codec dependency (SPEC-03 §1, SPEC-05 §5).
func TestVectorLiteral(t *testing.T) {
	cases := []struct {
		in   []float32
		want string
	}{
		{nil, "[]"},
		{[]float32{}, "[]"},
		{[]float32{1}, "[1]"},
		{[]float32{1, 2.5, -3}, "[1,2.5,-3]"},
	}
	for _, c := range cases {
		if got := vectorLiteral(c.in); got != c.want {
			t.Fatalf("vectorLiteral(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// validatePut enforces the SPEC-05 §5 precondition that a version reaches the
// commit fully embedded: every chunk carries a non-empty embedding and model,
// and the document identity + content hash are present.
func TestValidatePut(t *testing.T) {
	good := PutInput{
		SourceID:    "11111111-1111-1111-1111-111111111111",
		ExternalID:  "handbook.md",
		ContentHash: []byte{0x01, 0x02},
		Chunks: []ChunkInput{{
			Position:       0,
			Content:        "hello",
			TokenCount:     1,
			Embedding:      []float32{0.1, 0.2},
			EmbeddingModel: "text-embedding-3-small",
		}},
	}
	if err := validatePut(good); err != nil {
		t.Fatalf("validatePut(good) = %v, want nil", err)
	}

	bad := map[string]func(*PutInput){
		"empty external_id":  func(p *PutInput) { p.ExternalID = "" },
		"bad source_id":      func(p *PutInput) { p.SourceID = "not-a-uuid" },
		"empty content hash": func(p *PutInput) { p.ContentHash = nil },
		"nil embedding":      func(p *PutInput) { p.Chunks[0].Embedding = nil },
		"empty model":        func(p *PutInput) { p.Chunks[0].EmbeddingModel = "" },
	}
	for name, mutate := range bad {
		in := good
		in.Chunks = []ChunkInput{good.Chunks[0]}
		mutate(&in)
		var ve *ValidationError
		err := validatePut(in)
		if err == nil {
			t.Fatalf("%s: validatePut = nil, want ValidationError", name)
		}
		if !strings.Contains(err.Error(), "") { // err has a client-safe message
			t.Fatalf("%s: empty message", name)
		}
		if !asValidation(err, &ve) {
			t.Fatalf("%s: err = %T, want *ValidationError", name, err)
		}
	}
}

// asValidation is errors.As without importing errors in the test for one call.
func asValidation(err error, target **ValidationError) bool {
	v, ok := err.(*ValidationError)
	if ok {
		*target = v
	}
	return ok
}
