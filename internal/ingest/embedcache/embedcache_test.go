package embedcache

import (
	"context"
	"math"
	"testing"
)

// parseVector is the inverse of pgvector's embedding::text output, which
// comes back as "[a,b,c]" with no spaces (see Lookup's query in embedcache.go).
func TestParseVectorRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []float32
	}{
		{"one element", "[0.5]", []float32{0.5}},
		{"many elements", "[0.1,0.2,0.3,-1,42.25]", []float32{0.1, 0.2, 0.3, -1, 42.25}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseVector(c.in)
			if err != nil {
				t.Fatalf("parseVector(%q) error = %v, want nil", c.in, err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("parseVector(%q) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if diff := math.Abs(float64(got[i] - c.want[i])); diff > 1e-6 {
					t.Fatalf("parseVector(%q)[%d] = %v, want %v", c.in, i, got[i], c.want[i])
				}
			}
		})
	}
}

// parseVector must return an error, not panic, on malformed input.
func TestParseVectorRejectsMalformed(t *testing.T) {
	for _, in := range []string{"not-a-vector", "[1,,2]"} {
		if _, err := parseVector(in); err == nil {
			t.Fatalf("parseVector(%q) error = nil, want error", in)
		}
	}
}

// Lookup's empty-hashes guard must return before touching the DB: a nil
// *tenant.DB is safe here only because that guard runs first.
func TestLookupEmptyInputNoQuery(t *testing.T) {
	cache := NewPgCache()
	for name, hashes := range map[string][][]byte{"nil": nil, "empty slice": {}} {
		t.Run(name, func(t *testing.T) {
			got, err := cache.Lookup(context.Background(), nil, "some-model", hashes)
			if err != nil {
				t.Fatalf("Lookup(%s) error = %v, want nil", name, err)
			}
			if len(got) != 0 {
				t.Fatalf("Lookup(%s) = %v, want empty map", name, got)
			}
		})
	}
}
