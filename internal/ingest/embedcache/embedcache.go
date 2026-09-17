// Package embedcache reuses an existing chunk embedding for a chunk whose
// embed-text is byte-identical to one already embedded under the same model
// (chunk-level drift, SPEC-05 §1). It owns exactly the reuse lookup — the sink
// asks which chunk hashes are already known and skips re-embedding those.
package embedcache

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// Cache returns, for chunk-content hashes, the vector an existing chunk already
// holds for (hash, model). Hashes with no existing embedding are absent.
type Cache interface {
	Lookup(ctx context.Context, db *tenant.DB, model string, hashes [][]byte) (map[string][]float32, error)
}

type pgCache struct{}

// NewPgCache returns the production Cache over a tenant database.
func NewPgCache() Cache { return pgCache{} }

func (pgCache) Lookup(ctx context.Context, db *tenant.DB, model string, hashes [][]byte) (map[string][]float32, error) {
	if len(hashes) == 0 {
		return map[string][]float32{}, nil
	}
	// embedding::text avoids a pgvector codec dependency for one column (same
	// call the store makes on write, see internal/documents/put.go's
	// vectorLiteral); the text form round-trips through database/sql-style
	// scanning without registering the pgvector type.
	rows, err := db.Query(ctx,
		`select distinct on (content_hash) content_hash, embedding::text
		 from chunks where content_hash = any($1) and embedding_model = $2`,
		hashes, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][]float32, len(hashes))
	for rows.Next() {
		var h []byte
		var vecText string
		if err := rows.Scan(&h, &vecText); err != nil {
			return nil, err
		}
		vec, err := parseVector(vecText)
		if err != nil {
			return nil, fmt.Errorf("embedcache: parse embedding for hash %x: %w", h, err)
		}
		out[hex.EncodeToString(h)] = vec
	}
	return out, rows.Err()
}

// parseVector reads a pgvector text literal ("[a,b,c]") back into a []float32,
// the inverse of internal/documents/put.go's vectorLiteral.
func parseVector(s string) ([]float32, error) {
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return []float32{}, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(p, 32)
		if err != nil {
			return nil, err
		}
		out[i] = float32(f)
	}
	return out, nil
}
