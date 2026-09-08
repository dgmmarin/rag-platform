package worker

import "github.com/rag-platform/ragctl/internal/ingest/ingestdoc"

// parseTenantSettings extracts the ingest-pipeline fields from a tenant's resolved
// settings document (SPEC-02 §5) into the shape both the sync sink and the embedder
// factory consume. It mirrors ingestdoc's own settings parsing (the ingest_document
// handler parses settings itself); the sync path needs the same view to build its
// sink + embedder. Missing/mistyped fields fall back to zero, which the sink and
// chunker treat as their SPEC-05 defaults. JSON numbers decode as float64.
func parseTenantSettings(doc map[string]any) ingestdoc.Settings {
	var s ingestdoc.Settings
	if emb, ok := doc["embedding"].(map[string]any); ok {
		s.EmbeddingProvider, _ = emb["provider"].(string)
		s.EmbeddingModel, _ = emb["model"].(string)
		s.EmbeddingDim = toInt(emb["dim"])
	}
	if ch, ok := doc["chunking"].(map[string]any); ok {
		s.ChunkTarget = toInt(ch["target_tokens"])
		s.ChunkOverlap = toInt(ch["overlap_tokens"])
	}
	if allowed, ok := doc["providers_allowed"].([]any); ok {
		for _, a := range allowed {
			if p, ok := a.(string); ok {
				s.ProvidersAllowed = append(s.ProvidersAllowed, p)
			}
		}
	}
	return s
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
