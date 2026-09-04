package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// openAI speaks the OpenAI POST /v1/chat/completions API, which vLLM, Ollama and
// other OpenAI-compatible servers mirror — the only difference is baseURL, so one
// implementation serves "openai" and "openai-compatible" (ADR-0053). It is a plain
// net/http + encoding/json client with no vendor SDK (ADR-0002), consistent with
// internal/ingest/embed's OpenAI-compatible embedder.
type openAI struct {
	httpc      *http.Client
	baseURL    string
	apiKey     string
	propagator propagation.TextMapPropagator
}

func newOpenAI(c Config, baseURL string) *openAI {
	return &openAI{
		httpc:      c.HTTPClient,
		baseURL:    baseURL,
		apiKey:     c.APIKey,
		propagator: otel.GetTextMapPropagator(),
	}
}

// chatBody builds the request JSON. System is flattened into a leading system
// message (the OpenAI schema has no separate system field). Sampling params are
// included only when set; reasoning_effort maps Request.Effort.
func (p *openAI) chatBody(req Request, stream bool) ([]byte, error) {
	msgs := make([]map[string]string, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]string{"role": string(m.Role), "content": m.Content})
	}
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"messages":   msgs,
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.Effort != "" {
		body["reasoning_effort"] = req.Effort
	}
	if stream {
		body["stream"] = true
		// Ask for a final usage chunk (OpenAI omits usage from stream by default).
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: openai: marshal request: %w", err)
	}
	return b, nil
}

func (p *openAI) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	p.propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))
	return req, nil
}

func (p *openAI) complete(ctx context.Context, req Request) (Response, error) {
	body, err := p.chatBody(req, false)
	if err != nil {
		return Response{}, err
	}
	hreq, err := p.newRequest(ctx, body)
	if err != nil {
		return Response{}, err
	}
	resp, err := p.httpc.Do(hreq)
	if err != nil {
		return Response{}, &transientError{err: fmt.Errorf("llm: openai: request: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Response{}, p.statusError(resp)
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("llm: openai: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("llm: openai: response has no choices")
	}
	return Response{
		Text:         out.Choices[0].Message.Content,
		Usage:        Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens},
		FinishReason: normalizeFinish(out.Choices[0].FinishReason),
		Model:        out.Model,
	}, nil
}

func (p *openAI) openStream(ctx context.Context, req Request) (Stream, error) {
	body, err := p.chatBody(req, true)
	if err != nil {
		return nil, err
	}
	hreq, err := p.newRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	resp, err := p.httpc.Do(hreq)
	if err != nil {
		return nil, &transientError{err: fmt.Errorf("llm: openai: request: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, p.statusError(resp)
	}
	return &openAIStream{body: resp.Body, sc: bufio.NewScanner(resp.Body)}, nil
}

// statusError classifies a non-2xx response into a transient (429/5xx, retryable)
// or terminal error, carrying only the sanitised status and the provider's error
// snippet — never the request body (C-4).
func (p *openAI) statusError(resp *http.Response) error {
	msg := fmt.Errorf("llm: openai: status %d: %s", resp.StatusCode, snippet(resp.Body))
	if httpTransient(resp.StatusCode) {
		return &transientError{err: msg, retryAfter: retryAfter(resp.Header)}
	}
	return msg
}

// openAIStream reads the SSE chat/completions stream. Each `data:` line is a chunk
// with choices[].delta.content (text) and, on the last chunks, finish_reason and a
// usage object (include_usage). Recv returns text-delta events and one terminal
// Done event carrying the final usage/finish, then io.EOF.
type openAIStream struct {
	body io.ReadCloser
	sc   *bufio.Scanner

	usage       Usage
	finish      string
	doneEmitted bool
}

func (s *openAIStream) Recv() (Event, error) {
	if s.sc == nil {
		return Event{}, io.EOF
	}
	for s.sc.Scan() {
		line := strings.TrimSpace(s.sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return s.terminal()
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return Event{}, fmt.Errorf("llm: openai: decode stream chunk: %w", err)
		}
		if chunk.Usage != nil {
			s.usage = Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		if len(chunk.Choices) > 0 {
			if fr := chunk.Choices[0].FinishReason; fr != "" {
				s.finish = normalizeFinish(fr)
			}
			if c := chunk.Choices[0].Delta.Content; c != "" {
				return Event{TextDelta: c}, nil
			}
		}
		// no text in this chunk (role preamble / finish / usage-only): keep reading
	}
	if err := s.sc.Err(); err != nil {
		return Event{}, &transientError{err: fmt.Errorf("llm: openai: read stream: %w", err)}
	}
	return s.terminal()
}

func (s *openAIStream) terminal() (Event, error) {
	if s.doneEmitted {
		return Event{}, io.EOF
	}
	s.doneEmitted = true
	return Event{Done: true, Usage: s.usage, FinishReason: s.finish}, nil
}

func (s *openAIStream) Close() error {
	s.sc = nil
	if s.body != nil {
		err := s.body.Close()
		s.body = nil
		return err
	}
	return nil
}
