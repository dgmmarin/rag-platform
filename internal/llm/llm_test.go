package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- test helpers --------------------------------------------------------------

// drain reads a Stream to completion, returning the concatenated text deltas and
// the final done event (usage + finish reason).
func drain(t *testing.T, s Stream) (string, Event) {
	t.Helper()
	defer func() { _ = s.Close() }()
	var text strings.Builder
	var done Event
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return text.String(), done
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Done {
			done = ev
			continue
		}
		text.WriteString(ev.TextDelta)
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func sse(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, l := range lines {
		_, _ = io.WriteString(w, l)
		if fl != nil {
			fl.Flush()
		}
	}
}

// --- OpenAI (raw HTTP chat/completions) fixture server -------------------------

func openAINonStreamBody() string {
	return `{"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":5}}`
}

func openAIStreamLines() []string {
	return []string{
		`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":" there"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5}}` + "\n\n",
		"data: [DONE]\n\n",
	}
}

// --- Anthropic (Messages API) fixture server -----------------------------------

func anthropicNonStreamBody() string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5",` +
		`"content":[{"type":"text","text":"Hi from Claude"}],"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":9,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`
}

func anthropicStreamLines() []string {
	return []string{
		"event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n\n",
		"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi from"}}` + "\n\n",
		"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" Claude"}}` + "\n\n",
		"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n",
		"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}` + "\n\n",
		"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n",
	}
}

// providerCase drives the same behavioural assertions against each provider.
type providerCase struct {
	name       string
	provider   string
	nonStream  func(w http.ResponseWriter)
	stream     func(w http.ResponseWriter)
	wantText   string
	wantStream string
	wantIn     int
	wantOut    int
	wantFinish string
}

func cases() []providerCase {
	openai := func(name, prov string) providerCase {
		return providerCase{
			name:       name,
			provider:   prov,
			nonStream:  func(w http.ResponseWriter) { writeJSON(w, openAINonStreamBody()) },
			stream:     func(w http.ResponseWriter) { sse(w, openAIStreamLines()...) },
			wantText:   "Hello there",
			wantStream: "Hello there",
			wantIn:     12,
			wantOut:    5,
			wantFinish: "stop",
		}
	}
	return []providerCase{
		openai("openai", "openai"),
		openai("openai-compatible", "openai-compatible"),
		{
			name:       "anthropic",
			provider:   "anthropic",
			nonStream:  func(w http.ResponseWriter) { writeJSON(w, anthropicNonStreamBody()) },
			stream:     func(w http.ResponseWriter) { sse(w, anthropicStreamLines()...) },
			wantText:   "Hi from Claude",
			wantStream: "Hi from Claude",
			wantIn:     9,
			wantOut:    3,
			wantFinish: "stop",
		},
	}
}

func newProvider(t *testing.T, prov, baseURL string) Provider {
	t.Helper()
	p, err := New(Config{
		Provider: prov,
		Model:    "test-model",
		Allowed:  []string{prov},
		APIKey:   "test-key",
		BaseURL:  baseURL,
	})
	if err != nil {
		t.Fatalf("New(%s): %v", prov, err)
	}
	return p
}

func TestComplete_PerProvider(t *testing.T) {
	for _, tc := range cases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.nonStream(w)
			}))
			defer srv.Close()

			p := newProvider(t, tc.provider, srv.URL)
			resp, err := p.Complete(context.Background(), Request{
				System:    "you are a test",
				Messages:  []Message{{Role: RoleUser, Content: "hi"}},
				MaxTokens: 64,
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Text != tc.wantText {
				t.Errorf("text = %q, want %q", resp.Text, tc.wantText)
			}
			if resp.Usage.InputTokens != tc.wantIn || resp.Usage.OutputTokens != tc.wantOut {
				t.Errorf("usage = %+v, want in=%d out=%d", resp.Usage, tc.wantIn, tc.wantOut)
			}
			if resp.FinishReason != tc.wantFinish {
				t.Errorf("finish = %q, want %q", resp.FinishReason, tc.wantFinish)
			}
		})
	}
}

func TestStream_PerProvider(t *testing.T) {
	for _, tc := range cases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.stream(w)
			}))
			defer srv.Close()

			p := newProvider(t, tc.provider, srv.URL)
			s, err := p.Stream(context.Background(), Request{
				Messages:  []Message{{Role: RoleUser, Content: "hi"}},
				MaxTokens: 64,
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			text, done := drain(t, s)
			if text != tc.wantStream {
				t.Errorf("stream text = %q, want %q", text, tc.wantStream)
			}
			if !done.Done {
				t.Fatalf("no done event received")
			}
			if done.Usage.OutputTokens != tc.wantOut {
				t.Errorf("stream out tokens = %d, want %d", done.Usage.OutputTokens, tc.wantOut)
			}
			if tc.provider == "anthropic" && done.Usage.InputTokens != tc.wantIn {
				t.Errorf("stream in tokens = %d, want %d", done.Usage.InputTokens, tc.wantIn)
			}
			if done.FinishReason != tc.wantFinish {
				t.Errorf("stream finish = %q, want %q", done.FinishReason, tc.wantFinish)
			}
		})
	}
}

// TestComplete_RequestShape asserts the OpenAI provider flattens System into a
// leading system message and sends model/max_tokens/messages.
func TestComplete_RequestShape_OpenAI(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, openAINonStreamBody())
	}))
	defer srv.Close()

	p := newProvider(t, "openai", srv.URL)
	if _, err := p.Complete(context.Background(), Request{
		Model:     "gpt-4o",
		System:    "system-instruction",
		Messages:  []Message{{Role: RoleUser, Content: "question"}},
		MaxTokens: 128,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got["model"] != "gpt-4o" {
		t.Errorf("model = %v", got["model"])
	}
	if got["max_tokens"].(float64) != 128 {
		t.Errorf("max_tokens = %v", got["max_tokens"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2 (system+user)", msgs)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "system-instruction" {
		t.Errorf("first message = %v, want system/system-instruction", first)
	}
}

func TestRetry_TransientThenSuccess(t *testing.T) {
	for _, tc := range cases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := atomic.AddInt32(&calls, 1)
				if n <= 2 { // two transient failures, then succeed
					code := http.StatusInternalServerError
					if n == 2 {
						code = http.StatusTooManyRequests
					}
					w.WriteHeader(code)
					_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
					return
				}
				tc.nonStream(w)
			}))
			defer srv.Close()

			p, err := New(Config{
				Provider: tc.provider, Model: "m", Allowed: []string{tc.provider},
				APIKey: "test-key", BaseURL: srv.URL, MaxRetries: 3,
				BreakerCooldown: time.Millisecond,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			resp, err := p.Complete(context.Background(), Request{
				Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 32,
			})
			if err != nil {
				t.Fatalf("Complete after retries: %v", err)
			}
			if resp.Text != tc.wantText {
				t.Errorf("text = %q, want %q", resp.Text, tc.wantText)
			}
			if got := atomic.LoadInt32(&calls); got != 3 {
				t.Errorf("server calls = %d, want 3", got)
			}
		})
	}
}

func TestTerminalError_NotRetried_Sanitized(t *testing.T) {
	const secretKey = "sk-super-secret-key"
	const promptText = "confidential-user-prompt"
	for _, tc := range cases() {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
			}))
			defer srv.Close()

			p, err := New(Config{
				Provider: tc.provider, Model: "m", Allowed: []string{tc.provider},
				APIKey: secretKey, BaseURL: srv.URL, MaxRetries: 3,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = p.Complete(context.Background(), Request{
				System:    promptText,
				Messages:  []Message{{Role: RoleUser, Content: promptText}},
				MaxTokens: 32,
			})
			if err == nil {
				t.Fatal("expected error on 400")
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Errorf("server calls = %d, want 1 (400 not retried)", got)
			}
			if strings.Contains(err.Error(), secretKey) {
				t.Errorf("error leaked API key: %v", err)
			}
			if strings.Contains(err.Error(), promptText) {
				t.Errorf("error leaked prompt content: %v", err)
			}
		})
	}
}

func TestAllowlist_FailsClosed(t *testing.T) {
	_, err := New(Config{Provider: "anthropic", Model: "m", Allowed: []string{"openai"}, APIKey: "k"})
	if !errors.Is(err, ErrProviderNotAllowed) {
		t.Fatalf("err = %v, want ErrProviderNotAllowed", err)
	}
	// empty allowlist also fails closed
	_, err = New(Config{Provider: "anthropic", Model: "m", Allowed: nil, APIKey: "k"})
	if !errors.Is(err, ErrProviderNotAllowed) {
		t.Fatalf("empty allowlist err = %v, want ErrProviderNotAllowed", err)
	}
}

func TestUnknownProvider(t *testing.T) {
	_, err := New(Config{Provider: "made-up", Model: "m", Allowed: []string{"made-up"}, APIKey: "k"})
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("err = %v, want ErrUnknownProvider", err)
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, err := New(Config{
		Provider: "openai", Model: "m", Allowed: []string{"openai"}, APIKey: "k",
		BaseURL: srv.URL, MaxRetries: 0, BreakerThreshold: 2, BreakerCooldown: time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 8}
	for i := 0; i < 2; i++ {
		if _, err := p.Complete(context.Background(), req); err == nil {
			t.Fatalf("call %d: expected error", i)
		}
	}
	// third call should short-circuit without hitting the server
	_, err = p.Complete(context.Background(), req)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server calls = %d, want 2 (breaker short-circuits the 3rd)", got)
	}
}

func TestModelAllowlist_FailsClosed(t *testing.T) {
	// A configured model outside the tenant's model allowlist is rejected at build.
	_, err := New(Config{
		Provider: "anthropic", Model: "claude-opus-5", Allowed: []string{"anthropic"},
		AllowedModels: []string{"claude-sonnet-5", "claude-haiku-4-5"}, APIKey: "k",
	})
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("err = %v, want ErrModelNotAllowed", err)
	}
}

func TestModelAllowlist_WildcardAndExactAllow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAINonStreamBody())
	}))
	defer srv.Close()
	// "gpt-*" wildcard admits gpt-4o; exact entry admits claude-sonnet-5.
	p, err := New(Config{
		Provider: "openai", Model: "gpt-4o", Allowed: []string{"openai"},
		AllowedModels: []string{"gpt-*", "claude-sonnet-5"}, APIKey: "k", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New with allowed model: %v", err)
	}
	if _, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 8}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestModelAllowlist_PerRequestOverrideRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAINonStreamBody())
	}))
	defer srv.Close()
	p, err := New(Config{
		Provider: "openai", Model: "gpt-4o", Allowed: []string{"openai"},
		AllowedModels: []string{"gpt-4o"}, APIKey: "k", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A per-request model outside the allowlist fails closed at call time.
	_, err = p.Complete(context.Background(), Request{Model: "gpt-5-ultra", Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 8})
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("err = %v, want ErrModelNotAllowed", err)
	}
}

func TestModelAllowlist_EmptyMeansNoModelRestriction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAINonStreamBody())
	}))
	defer srv.Close()
	// No models_allowed configured: provider allowlist still applies, model is free.
	p, err := New(Config{Provider: "openai", Model: "any-model", Allowed: []string{"openai"}, APIKey: "k", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 8}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestFactory_SelectsKeyPerProvider(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, openAINonStreamBody())
	}))
	defer srv.Close()

	f := Factory{Keys: Keys{OpenAI: "openai-platform-key", Anthropic: "anthropic-platform-key", OpenAIBaseURL: srv.URL}}
	p, err := f.Provider("openai-compatible", "m", []string{"openai-compatible"}, nil)
	if err != nil {
		t.Fatalf("Factory.Provider: %v", err)
	}
	if _, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}, MaxTokens: 8}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer openai-platform-key" {
		t.Errorf("auth = %q, want the openai platform key", gotAuth)
	}
}

// ensure the interfaces are wired (compile-time guard, plus a readable failure).
func TestProviderInterfaceSatisfied(t *testing.T) {
	for _, prov := range []string{"openai", "openai-compatible", "anthropic"} {
		if _, err := New(Config{Provider: prov, Model: "m", Allowed: []string{prov}, APIKey: "k"}); err != nil {
			t.Errorf("New(%s): %v", prov, err)
		}
	}
}
