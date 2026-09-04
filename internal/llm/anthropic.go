package llm

import (
	"context"
	"errors"
	"fmt"
	"io"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// anthropicProvider talks to the Anthropic Messages API through the official
// anthropic-sdk-go (the user's approved dependency for this provider; OpenAI stays
// raw HTTP). The SDK's own retry is disabled (WithMaxRetries(0)) so this package's
// resilient wrapper is the single retry authority — identical retry/breaker
// behaviour to the OpenAI provider (ADR-0053, NFR-REL-04).
//
// Sampling params are deliberately not set: current Claude models reject
// temperature/top_p. Thinking/effort is not hardcoded — the answering layer
// (08.5) owns that policy; Request.Effort is a no-op for this SDK version (see
// ADR-0053).
type anthropicProvider struct {
	client anthropic.Client
}

func newAnthropic(c Config) *anthropicProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(c.APIKey),
		option.WithMaxRetries(0), // resilient wrapper owns retries
	}
	if c.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(c.BaseURL))
	}
	if c.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTPClient))
	}
	return &anthropicProvider{client: anthropic.NewClient(opts...)}
}

func (p *anthropicProvider) params(req Request) anthropic.MessageNewParams {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  toAnthropicMessages(req.Messages),
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	return params
}

func toAnthropicMessages(msgs []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		block := anthropic.NewTextBlock(m.Content)
		if m.Role == RoleAssistant {
			out = append(out, anthropic.NewAssistantMessage(block))
		} else {
			out = append(out, anthropic.NewUserMessage(block))
		}
	}
	return out
}

func (p *anthropicProvider) complete(ctx context.Context, req Request) (Response, error) {
	msg, err := p.client.Messages.New(ctx, p.params(req))
	if err != nil {
		return Response{}, classifyAnthropic(err)
	}
	var text string
	for _, block := range msg.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	return Response{
		Text:         text,
		Usage:        Usage{InputTokens: int(msg.Usage.InputTokens), OutputTokens: int(msg.Usage.OutputTokens)},
		FinishReason: normalizeFinish(string(msg.StopReason)),
		Model:        string(msg.Model),
	}, nil
}

func (p *anthropicProvider) openStream(ctx context.Context, req Request) (Stream, error) {
	// The SDK surfaces stream errors lazily (on the first Next), so establishment
	// failures cannot be caught here to drive a retry — they surface on the first
	// Recv instead. ponytail: the circuit breaker still guards streams; Complete
	// gets full retry. A future SDK exposing eager stream errors would let the
	// resilient wrapper retry stream establishment like the OpenAI provider does.
	stream := p.client.Messages.NewStreaming(ctx, p.params(req))
	return &anthropicStream{stream: stream}, nil
}

// classifyAnthropic maps an SDK error onto transient (429/5xx, retryable) or
// terminal, exposing ONLY the HTTP status — never the SDK error string, which can
// echo the request URL/body. Non-API errors (transport/connection) are transient.
func classifyAnthropic(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		m := fmt.Errorf("llm: anthropic: status %d", apiErr.StatusCode)
		if httpTransient(apiErr.StatusCode) {
			return &transientError{err: m}
		}
		return m
	}
	return &transientError{err: errors.New("llm: anthropic: request failed")}
}

// anthropicStream adapts the SDK SSE stream to the Stream interface. It loops over
// SDK events internally, returning only text deltas and one terminal Done event
// (with input tokens from message_start and output tokens/stop reason from
// message_delta), then io.EOF.
type anthropicStream struct {
	stream *ssestream.Stream[anthropic.MessageStreamEventUnion]

	usage       Usage
	finish      string
	doneEmitted bool
}

func (s *anthropicStream) Recv() (Event, error) {
	if s.stream == nil {
		return Event{}, io.EOF
	}
	for s.stream.Next() {
		evt := s.stream.Current()
		switch evt.Type {
		case "message_start":
			s.usage.InputTokens = int(evt.Message.Usage.InputTokens)
		case "content_block_delta":
			if evt.Delta.Text != "" {
				return Event{TextDelta: evt.Delta.Text}, nil
			}
		case "message_delta":
			if evt.Usage.OutputTokens != 0 {
				s.usage.OutputTokens = int(evt.Usage.OutputTokens)
			}
			if evt.Delta.StopReason != "" {
				s.finish = normalizeFinish(string(evt.Delta.StopReason))
			}
		case "message_stop":
			return s.terminal()
		}
	}
	if err := s.stream.Err(); err != nil {
		return Event{}, classifyAnthropic(err)
	}
	return s.terminal()
}

func (s *anthropicStream) terminal() (Event, error) {
	if s.doneEmitted {
		return Event{}, io.EOF
	}
	s.doneEmitted = true
	return Event{Done: true, Usage: s.usage, FinishReason: s.finish}, nil
}

func (s *anthropicStream) Close() error {
	if s.stream != nil {
		err := s.stream.Close()
		s.stream = nil
		return err
	}
	return nil
}
