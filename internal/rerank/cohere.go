package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// cohere speaks the Cohere v2 rerank API: POST /v2/rerank with {model, query,
// documents:[...]}, Bearer auth, response {results:[{index, relevance_score}]}
// already ordered by relevance descending. It is a plain net/http + encoding/json
// client (no vendor SDK, C-2/ADR-0002), consistent with internal/ingest/embed's
// Cohere embedder. The doer supplies retry/backoff; the breaker guards a provider
// outage. top_n is deliberately NOT sent — the reranker must return EVERY candidate
// scored so the service can re-order the full top_n set (the service already trimmed
// the fused set to settings.reranker.top_n before calling).
type cohere struct {
	doer    *doer
	breaker *breaker
	baseURL string
	apiKey  string
	model   string
}

// cohereResults is the subset of the v2 rerank response used: each result carries
// the input document index and its relevance score.
type cohereResults struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

func (c *cohere) Rerank(ctx context.Context, query string, docs []Doc) ([]Scored, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Text
	}
	body, err := json.Marshal(map[string]any{
		"model":     c.model,
		"query":     query,
		"documents": texts,
	})
	if err != nil {
		return nil, fmt.Errorf("rerank: cohere: marshal request: %w", err)
	}

	if err := c.breaker.allow(); err != nil {
		return nil, err
	}
	raw, err := c.doer.do(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v2/rerank", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		return req, nil
	})
	c.breaker.record(err)
	if err != nil {
		return nil, err
	}

	var out cohereResults
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("rerank: cohere: decode response: %w", err)
	}
	scored := make([]Scored, 0, len(out.Results))
	for _, r := range out.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			return nil, fmt.Errorf("rerank: cohere: result index %d out of range (%d docs)", r.Index, len(docs))
		}
		scored = append(scored, Scored{ID: docs[r.Index].ID, Score: r.RelevanceScore})
	}
	// Cohere returns results in descending relevance order; enforce it defensively
	// (stable) so the service can rely on Score-descending regardless of provider
	// quirks, then complete any omitted docs at the tail.
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	return orderMissingLast(docs, scored), nil
}
