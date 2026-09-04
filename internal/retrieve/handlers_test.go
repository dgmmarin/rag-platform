package retrieve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/tenant"
)

func newTestService(retr func(context.Context, *tenant.DB, Params) ([]Result, error), embErr error) *Service {
	emb := &fakeEmbedder{vec: []float32{1, 0, 0, 0, 0, 0, 0, 0}, err: embErr}
	return &Service{
		Resolver:  &fakeResolver{},
		Settings:  fakeSettings{doc: newTestSettingsDoc()},
		Embedder:  &fakeFactory{emb: emb},
		Retriever: retr,
	}
}

func postRetrieve(t *testing.T, h *Handlers, body string, withTenant bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/retrieve", strings.NewReader(body))
	if withTenant {
		r = r.WithContext(tenant.WithTenantID(r.Context(), testTenantID()))
	}
	rr := httptest.NewRecorder()
	h.Retrieve(rr, r)
	return rr
}

// TestRetrieveHandlerGoldenPath: a valid request returns 200 with ranked chunks
// carrying the SPEC-06/07 citation metadata (id, document_id, ..., score).
func TestRetrieveHandlerGoldenPath(t *testing.T) {
	var got Params
	svc := newTestService(func(_ context.Context, _ *tenant.DB, p Params) ([]Result, error) {
		got = p
		return []Result{{
			ChunkID: "c1", DocumentID: "d1", SourceID: "s1", Content: "reset the X200",
			URI: "https://docs.acme.com/x200", Title: "X200 Guide",
			HeadingPath: []string{"Reset"}, Metadata: json.RawMessage(`{"team":"support"}`), Score: 0.0328,
		}}, nil
	}, nil)
	h := NewHandlers(svc)

	rr := postRetrieve(t, h, `{"query":"reset X200","top_k":4,"filters":{"source_ids":["s1"],"uri_prefix":"https://docs.acme.com/"}}`, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Chunks []struct {
			ID          string          `json:"id"`
			DocumentID  string          `json:"document_id"`
			SourceID    string          `json:"source_id"`
			Content     string          `json:"content"`
			URI         string          `json:"uri"`
			Title       string          `json:"title"`
			HeadingPath []string        `json:"heading_path"`
			Metadata    json.RawMessage `json:"metadata"`
			Score       float64         `json:"score"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rr.Body.String())
	}
	if len(resp.Chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(resp.Chunks))
	}
	c := resp.Chunks[0]
	if c.ID != "c1" || c.DocumentID != "d1" || c.SourceID != "s1" || c.URI != "https://docs.acme.com/x200" ||
		c.Title != "X200 Guide" || c.Content != "reset the X200" || c.Score != 0.0328 {
		t.Fatalf("chunk mismapped: %+v", c)
	}
	if len(c.HeadingPath) != 1 || c.HeadingPath[0] != "Reset" || string(c.Metadata) != `{"team":"support"}` {
		t.Fatalf("chunk heading/metadata mismapped: %+v", c)
	}
	// top_k and filters reached the retriever.
	if got.K != 4 {
		t.Fatalf("retriever K = %d, want 4", got.K)
	}
	if got.Filters.URIPrefix != "https://docs.acme.com/" || len(got.Filters.SourceIDs) != 1 || got.Filters.SourceIDs[0] != "s1" {
		t.Fatalf("filters not passed through: %+v", got.Filters)
	}
}

// TestRetrieveHandlerNoTenant: without a resolved tenant the handler is 401 (the
// scope middleware normally sets it; FR-ACC-03 — never a body/param).
func TestRetrieveHandlerNoTenant(t *testing.T) {
	h := NewHandlers(newTestService(func(context.Context, *tenant.DB, Params) ([]Result, error) { return nil, nil }, nil))
	rr := postRetrieve(t, h, `{"query":"x"}`, false)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	assertErrorCode(t, rr.Body.Bytes(), "unauthorized")
}

// TestRetrieveHandlerBadBody: malformed JSON is a 400 validation envelope.
func TestRetrieveHandlerBadBody(t *testing.T) {
	h := NewHandlers(newTestService(func(context.Context, *tenant.DB, Params) ([]Result, error) { return nil, nil }, nil))
	rr := postRetrieve(t, h, `{not json`, true)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	assertErrorCode(t, rr.Body.Bytes(), "validation")
}

// TestRetrieveHandlerEmptyQuery: a blank query is a 400 validation envelope.
func TestRetrieveHandlerEmptyQuery(t *testing.T) {
	h := NewHandlers(newTestService(func(context.Context, *tenant.DB, Params) ([]Result, error) { return nil, nil }, nil))
	rr := postRetrieve(t, h, `{"query":"  "}`, true)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr.Body.Bytes(), "validation")
}

// TestRetrieveHandlerEmbedFailureIsGeneric: a provider failure is a 500 internal
// envelope with a generic message — provider internals are never leaked.
func TestRetrieveHandlerEmbedFailureIsGeneric(t *testing.T) {
	h := NewHandlers(newTestService(func(context.Context, *tenant.DB, Params) ([]Result, error) {
		t.Fatal("retriever called despite embed failure")
		return nil, nil
	}, errLeaky))
	rr := postRetrieve(t, h, `{"query":"x"}`, true)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "secret") || strings.Contains(rr.Body.String(), "voyage") {
		t.Fatalf("response leaked provider internals: %s", rr.Body.String())
	}
	assertErrorCode(t, rr.Body.Bytes(), "internal")
}

var errLeaky = leakyErr{}

type leakyErr struct{}

func (leakyErr) Error() string { return "429 from voyage: secret-token abc123 leaked" }

func assertErrorCode(t *testing.T, body []byte, want string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, body)
	}
	if env.Error.Code != want {
		t.Fatalf("error code = %q, want %q; body=%s", env.Error.Code, want, body)
	}
}
