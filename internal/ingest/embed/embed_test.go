package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/ingest/embed"
)

// echoValue parses the numeric suffix of a text like "t7" so a fake provider can
// return a deterministic embedding ([float(7)]) and tests can assert that the
// vector for input i is [float(i)] — i.e. order is preserved across sorting and
// batch reassembly.
func echoValue(t *testing.T, text string) float64 {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimPrefix(text, "t"))
	if err != nil {
		t.Fatalf("bad echo text %q: %v", text, err)
	}
	return float64(n)
}

func texts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "t" + strconv.Itoa(i)
	}
	return out
}

func assertOrdered(t *testing.T, vecs [][]float32, n int) {
	t.Helper()
	if len(vecs) != n {
		t.Fatalf("got %d vectors, want %d", len(vecs), n)
	}
	for i, v := range vecs {
		if len(v) != 1 || v[0] != float32(i) {
			t.Fatalf("vector[%d] = %v, want [%d]", i, v, i)
		}
	}
}

// --- OpenAI / Voyage (openai-compatible schema) --------------------------------

func TestOpenAIEmbedRequestShapeAndOrder(t *testing.T) {
	var gotAuth, gotPath, gotModel string
	var gotInputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		gotInputs = body.Input
		// Reply with the data entries in REVERSE order to prove the provider
		// re-sorts by index.
		var data []map[string]any
		for i := len(body.Input) - 1; i >= 0; i-- {
			data = append(data, map[string]any{
				"index":     i,
				"embedding": []float64{echoValue(t, body.Input[i])},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  data,
			"usage": map[string]any{"total_tokens": 42},
		})
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"},
		APIKey: "sk-test", Model: "text-embedding-3-small", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := e.Embed(context.Background(), texts(3))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q", gotPath)
	}
	if gotModel != "text-embedding-3-small" {
		t.Errorf("model = %q", gotModel)
	}
	if len(gotInputs) != 3 {
		t.Errorf("inputs = %v", gotInputs)
	}
	assertOrdered(t, res.Vectors, 3)
	if res.Tokens != 42 {
		t.Errorf("tokens = %d, want 42", res.Tokens)
	}
}

func TestVoyageEmbedDefaultEndpointAndTokens(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		data := make([]map[string]any, len(body.Input))
		for i, in := range body.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{echoValue(t, in)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  data,
			"usage": map[string]any{"total_tokens": 11},
		})
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "voyage", Allowed: []string{"voyage"},
		APIKey: "k", Model: "voyage-3", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := e.Embed(context.Background(), texts(2))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q", gotPath)
	}
	assertOrdered(t, res.Vectors, 2)
	if res.Tokens != 11 {
		t.Errorf("tokens = %d, want 11", res.Tokens)
	}
}

func TestCohereEmbedRequestShapeAndTokens(t *testing.T) {
	var gotPath string
	var body struct {
		Model     string   `json:"model"`
		Texts     []string `json:"texts"`
		InputType string   `json:"input_type"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		embs := make([][]float64, len(body.Texts))
		for i, in := range body.Texts {
			embs[i] = []float64{echoValue(t, in)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": embs,
			"meta":       map[string]any{"billed_units": map[string]any{"input_tokens": 9}},
		})
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "cohere", Allowed: []string{"cohere"},
		APIKey: "k", Model: "embed-english-v3.0", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := e.Embed(context.Background(), texts(2))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/v1/embed" {
		t.Errorf("path = %q", gotPath)
	}
	if body.InputType != "search_document" {
		t.Errorf("input_type = %q, want search_document", body.InputType)
	}
	assertOrdered(t, res.Vectors, 2)
	if res.Tokens != 9 {
		t.Errorf("tokens = %d, want 9", res.Tokens)
	}
}

func TestTEIEmbedRequestShapeAndZeroTokens(t *testing.T) {
	var gotPath string
	var body struct {
		Inputs []string `json:"inputs"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		out := make([][]float64, len(body.Inputs))
		for i, in := range body.Inputs {
			out[i] = []float64{echoValue(t, in)}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "tei", Allowed: []string{"tei"}, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := e.Embed(context.Background(), texts(3))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/embed" {
		t.Errorf("path = %q", gotPath)
	}
	assertOrdered(t, res.Vectors, 3)
	if res.Tokens != 0 {
		t.Errorf("tokens = %d, want 0 (TEI reports none)", res.Tokens)
	}
}

// --- Batching ------------------------------------------------------------------

func TestBatchesRespectMaxTextsAndPreserveOrder(t *testing.T) {
	var reqSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		reqSizes = append(reqSizes, len(b.Input))
		data := make([]map[string]any, len(b.Input))
		for i, in := range b.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{echoValue(t, in)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  data,
			"usage": map[string]any{"total_tokens": len(b.Input)},
		})
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"}, APIKey: "k",
		Model: "m", BaseURL: srv.URL, MaxBatchTexts: 2, Concurrency: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := e.Embed(context.Background(), texts(5))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	assertOrdered(t, res.Vectors, 5)
	if res.Tokens != 5 {
		t.Errorf("tokens = %d, want 5 (summed across batches)", res.Tokens)
	}
	for _, s := range reqSizes {
		if s > 2 {
			t.Errorf("a batch had %d texts, exceeds MaxBatchTexts=2", s)
		}
	}
	if len(reqSizes) != 3 { // 2+2+1
		t.Errorf("made %d requests, want 3", len(reqSizes))
	}
}

func TestBatchesRespectTokenBudget(t *testing.T) {
	var reqSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		reqSizes = append(reqSizes, len(b.Input))
		data := make([]map[string]any, len(b.Input))
		for i, in := range b.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{echoValue(t, in)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"total_tokens": 0}})
	}))
	defer srv.Close()

	// Each text counts as 10 tokens; a 25-token budget admits 2 per batch.
	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"}, APIKey: "k", Model: "m",
		BaseURL: srv.URL, MaxBatchTexts: 96, MaxBatchTokens: 25, Concurrency: 1,
		CountTokens: func(string) int { return 10 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := e.Embed(context.Background(), texts(5))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	assertOrdered(t, res.Vectors, 5)
	for _, s := range reqSizes {
		if s > 2 {
			t.Errorf("batch of %d exceeds token budget (2 texts * 10 tokens <= 25)", s)
		}
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	var inFlight, maxInFlight int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt64(&inFlight, 1)
		for {
			old := atomic.LoadInt64(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt64(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		var b struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		data := make([]map[string]any, len(b.Input))
		for i, in := range b.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{echoValue(t, in)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"total_tokens": 0}})
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"}, APIKey: "k", Model: "m",
		BaseURL: srv.URL, MaxBatchTexts: 1, Concurrency: 2,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Embed(context.Background(), texts(8)); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := atomic.LoadInt64(&maxInFlight); got > 2 {
		t.Errorf("max in-flight batches = %d, want <= 2", got)
	}
}

// --- Retry / Retry-After -------------------------------------------------------

func TestRetriesTransientThenSucceeds(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var b struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		data := make([]map[string]any, len(b.Input))
		for i, in := range b.Input {
			data[i] = map[string]any{"index": i, "embedding": []float64{echoValue(t, in)}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"total_tokens": 3}})
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"}, APIKey: "k", Model: "m",
		BaseURL: srv.URL, MaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := e.Embed(context.Background(), texts(1))
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	assertOrdered(t, res.Vectors, 1)
	if atomic.LoadInt64(&calls) != 2 {
		t.Errorf("server calls = %d, want 2 (one 429 then success)", calls)
	}
}

func TestRetryAfterIsCancellableByContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600") // one hour
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"}, APIKey: "k", Model: "m",
		BaseURL: srv.URL, MaxRetries: 5,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = e.Embed(ctx, texts(1))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if time.Since(start) > time.Second {
		t.Errorf("Embed waited the full Retry-After instead of honouring context cancel")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// --- Circuit breaker -----------------------------------------------------------

func TestCircuitBreakerOpensAndShortCircuits(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"}, APIKey: "k", Model: "m",
		BaseURL: srv.URL, MaxRetries: 0, BreakerThreshold: 3, BreakerCooldown: time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Drive failures until the breaker trips.
	for i := 0; i < 3; i++ {
		if _, err := e.Embed(context.Background(), texts(1)); err == nil {
			t.Fatalf("attempt %d: expected error", i)
		}
	}
	callsBefore := atomic.LoadInt64(&calls)
	// Next call must short-circuit: ErrCircuitOpen, no new server hit.
	_, err = e.Embed(context.Background(), texts(1))
	if !errors.Is(err, embed.ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if atomic.LoadInt64(&calls) != callsBefore {
		t.Errorf("server was called while breaker open (%d -> %d)", callsBefore, atomic.LoadInt64(&calls))
	}
}

// --- Allowlist -----------------------------------------------------------------

func TestAllowlistRejectsNonAllowedProvider(t *testing.T) {
	_, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"voyage", "cohere"}, APIKey: "k", Model: "m",
	})
	if !errors.Is(err, embed.ErrProviderNotAllowed) {
		t.Fatalf("err = %v, want ErrProviderNotAllowed", err)
	}
}

func TestAllowlistFailsClosedWhenEmpty(t *testing.T) {
	_, err := embed.New(embed.Config{
		Provider: "openai", Allowed: nil, APIKey: "k", Model: "m",
	})
	if !errors.Is(err, embed.ErrProviderNotAllowed) {
		t.Fatalf("empty allowlist must fail closed; err = %v", err)
	}
}

func TestUnknownProviderRejected(t *testing.T) {
	_, err := embed.New(embed.Config{
		Provider: "bogus", Allowed: []string{"bogus"},
	})
	if !errors.Is(err, embed.ErrUnknownProvider) {
		t.Fatalf("err = %v, want ErrUnknownProvider", err)
	}
}

// --- Edge cases ----------------------------------------------------------------

func TestEmptyInputMakesNoRequest(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"}, APIKey: "k", Model: "m", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := e.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(res.Vectors) != 0 || res.Tokens != 0 {
		t.Errorf("empty input: got %d vectors, %d tokens", len(res.Vectors), res.Tokens)
	}
	if atomic.LoadInt64(&calls) != 0 {
		t.Errorf("empty input hit the server %d times", calls)
	}
}

// Guards that a provider surfaces a terminal (non-retryable) error, e.g. a 400,
// rather than retrying it forever.
func TestTerminalErrorNotRetried(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	e, err := embed.New(embed.Config{
		Provider: "openai", Allowed: []string{"openai"}, APIKey: "k", Model: "m",
		BaseURL: srv.URL, MaxRetries: 5,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Embed(context.Background(), texts(1)); err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("terminal 400 retried: %d calls, want 1", got)
	}
}
