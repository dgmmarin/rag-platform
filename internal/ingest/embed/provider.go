package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel"
)

// registry maps a provider name to a constructor. Adding a provider is a single
// entry here plus one file implementing Embedder — no change anywhere else
// (NFR-MNT-02, C-2: these are plain net/http + encoding/json clients, no vendor
// SDKs). The set is also the closed vocabulary the allowlist is checked against.
var registry = map[string]func(Config) Embedder{
	"openai": func(c Config) Embedder {
		return &openAICompatible{doer: newDoer(c, "openai"), baseURL: nonEmpty(c.BaseURL, "https://api.openai.com"), apiKey: c.APIKey, model: c.Model}
	},
	"voyage": func(c Config) Embedder {
		return &openAICompatible{doer: newDoer(c, "voyage"), baseURL: nonEmpty(c.BaseURL, "https://api.voyageai.com"), apiKey: c.APIKey, model: c.Model}
	},
	"cohere": func(c Config) Embedder {
		return &cohere{doer: newDoer(c, "cohere"), baseURL: nonEmpty(c.BaseURL, "https://api.cohere.com"), apiKey: c.APIKey, model: c.Model}
	},
	"tei": func(c Config) Embedder {
		return &tei{doer: newDoer(c, "tei"), baseURL: nonEmpty(c.BaseURL, "http://localhost:8080"), apiKey: c.APIKey}
	},
}

func newDoer(c Config, name string) *doer {
	return &doer{
		provider:   name,
		httpc:      c.HTTPClient,
		maxRetries: c.MaxRetries,
		tracer:     otel.Tracer("ingest/embed"),
		propagator: otel.GetTextMapPropagator(),
	}
}

func jsonBody(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal request: %w", err)
	}
	return b, nil
}

// --- OpenAI / Voyage (OpenAI-compatible embeddings schema) ---------------------

// openAICompatible speaks the OpenAI embeddings API, which Voyage mirrors: POST
// /v1/embeddings with {model, input:[...]}, Bearer auth, and a response of
// {data:[{index, embedding}], usage:{total_tokens}}. The data array may arrive
// out of order, so it is placed back by index.
type openAICompatible struct {
	doer    *doer
	baseURL string
	apiKey  string
	model   string
}

func (p *openAICompatible) Embed(ctx context.Context, texts []string) (Result, error) {
	if len(texts) == 0 {
		return Result{Vectors: [][]float32{}}, nil
	}
	body, err := jsonBody(map[string]any{"model": p.model, "input": texts})
	if err != nil {
		return Result{}, err
	}
	raw, err := p.doer.do(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
		return req, nil
	})
	if err != nil {
		return Result{}, err
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("embed: %s: decode response: %w", p.doer.provider, err)
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return Result{}, fmt.Errorf("embed: %s: embedding index %d out of range", p.doer.provider, d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	if err := ensureComplete(p.doer.provider, vecs); err != nil {
		return Result{}, err
	}
	return Result{Vectors: vecs, Tokens: out.Usage.TotalTokens}, nil
}

// --- Cohere --------------------------------------------------------------------

// cohere speaks the Cohere embed API: POST /v1/embed with {model, texts:[...],
// input_type}, Bearer auth, response {embeddings:[[...]], meta:{billed_units:
// {input_tokens}}}. input_type "search_document" is the correct asymmetric mode
// for indexing corpus content (queries use "search_query").
type cohere struct {
	doer    *doer
	baseURL string
	apiKey  string
	model   string
}

func (p *cohere) Embed(ctx context.Context, texts []string) (Result, error) {
	if len(texts) == 0 {
		return Result{Vectors: [][]float32{}}, nil
	}
	body, err := jsonBody(map[string]any{"model": p.model, "texts": texts, "input_type": "search_document"})
	if err != nil {
		return Result{}, err
	}
	raw, err := p.doer.do(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/embed", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
		return req, nil
	})
	if err != nil {
		return Result{}, err
	}
	// ponytail: default (no embedding_types) v1 response returns a bare
	// embeddings:[[...]] array in input order. Upgrade to embeddings.float when a
	// tenant pins embedding_types (e.g. int8/binary compression).
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
		Meta       struct {
			BilledUnits struct {
				InputTokens int `json:"input_tokens"`
			} `json:"billed_units"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("embed: cohere: decode response: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return Result{}, fmt.Errorf("embed: cohere: got %d embeddings for %d texts", len(out.Embeddings), len(texts))
	}
	if err := ensureComplete("cohere", out.Embeddings); err != nil {
		return Result{}, err
	}
	return Result{Vectors: out.Embeddings, Tokens: out.Meta.BilledUnits.InputTokens}, nil
}

// --- TEI (self-hosted text-embeddings-inference) -------------------------------

// tei speaks the self-hosted HuggingFace text-embeddings-inference native API:
// POST /embed with {inputs:[...]} and a response of a bare [[...]] array in input
// order. It reports no token usage (Tokens stays 0 — the sink falls back to the
// chunker's token counts). An apiKey, when set, is sent as a Bearer token for a
// gated deployment.
type tei struct {
	doer    *doer
	baseURL string
	apiKey  string
}

func (p *tei) Embed(ctx context.Context, texts []string) (Result, error) {
	if len(texts) == 0 {
		return Result{Vectors: [][]float32{}}, nil
	}
	body, err := jsonBody(map[string]any{"inputs": texts})
	if err != nil {
		return Result{}, err
	}
	raw, err := p.doer.do(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/embed", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if p.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		return req, nil
	})
	if err != nil {
		return Result{}, err
	}
	var out [][]float32
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("embed: tei: decode response: %w", err)
	}
	if len(out) != len(texts) {
		return Result{}, fmt.Errorf("embed: tei: got %d embeddings for %d texts", len(out), len(texts))
	}
	if err := ensureComplete("tei", out); err != nil {
		return Result{}, err
	}
	return Result{Vectors: out, Tokens: 0}, nil
}

// ensureComplete guards that every text got a non-empty vector, so a malformed
// or partial provider response cannot silently produce a nil embedding that the
// document store would then reject at commit (validatePut requires a vector).
func ensureComplete(provider string, vecs [][]float32) error {
	for i, v := range vecs {
		if len(v) == 0 {
			return fmt.Errorf("embed: %s: missing embedding for index %d", provider, i)
		}
	}
	return nil
}
