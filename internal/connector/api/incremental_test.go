package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rag-platform/ragctl/internal/connector"
)

// memState is an in-memory connector.StateStore for hermetic incremental tests (the
// DB-backed store has its own e2e). It records the last value Set per key.
type memState struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemState() *memState { return &memState{m: map[string]string{}} }

func (s *memState) Get(_ context.Context, k string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok, nil
}

func (s *memState) Set(_ context.Context, k, v string) error {
	s.mu.Lock()
	s.m[k] = v
	s.mu.Unlock()
	return nil
}

func (s *memState) get(k string) string {
	v, _, _ := s.Get(context.Background(), k)
	return v
}

// incFixture is a single-page endpoint whose items each carry updated_at. It filters
// by the updated_since query param (RFC3339 string compare, which is chronological
// for zulu timestamps) and records the last updated_since value it received, so a
// test can prove the connector sent (or withheld) the cursor.
type incFixture struct {
	mu           sync.Mutex
	items        []map[string]any
	lastSince    string
	sinceSeen    bool
	requestCount int
}

func (f *incFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("updated_since")
	f.mu.Lock()
	f.requestCount++
	f.lastSince = since
	f.sinceSeen = r.URL.Query().Has("updated_since")
	items := f.items
	f.mu.Unlock()

	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if since == "" || it["updated_at"].(string) > since {
			out = append(out, it)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": out})
}

func incEndpoint() endpoint {
	return endpoint{
		Name: "items", Path: "/items", Method: http.MethodGet,
		Pagination: pagination{Type: "none"}, ItemsPath: "$.data",
		IDPath: "$.id", UpdatedPath: "$.updated_at", IncrementalParam: "updated_since",
	}
}

func incConfig(t *testing.T, baseURL string) json.RawMessage {
	t.Helper()
	return mustConfig(t, apiConfig{
		BaseURL:   baseURL,
		Auth:      authConfig{Type: "bearer"},
		Endpoints: []endpoint{incEndpoint()},
	})
}

// TestIncrementalCursorRoundTrip is the AC's incremental golden path: a first
// incremental run with no stored cursor enumerates everything and stores the max
// updated_at; the second run sends that cursor as updated_since and only sees newer
// items, then advances the cursor.
func TestIncrementalCursorRoundTrip(t *testing.T) {
	restore := syncClient
	t.Cleanup(func() { syncClient = restore })

	fx := &incFixture{items: []map[string]any{
		{"id": "a", "updated_at": "2026-01-01T00:00:00Z"},
		{"id": "b", "updated_at": "2026-01-02T00:00:00Z"},
		{"id": "c", "updated_at": "2026-01-03T00:00:00Z"},
	}}
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	SetEgressClientForTest(srv.Client())

	state := newMemState()
	sourceID := uuid.New()
	run := func() connector.SyncRun {
		return connector.SyncRun{
			SourceID: sourceID, Config: incConfig(t, srv.URL),
			Creds: connector.Credentials(credsFor("bearer")), State: state, Full: false, Log: apiTestLogger(),
		}
	}

	// --- Run 1: no cursor yet -> no updated_since, all 3 items, cursor := max. ---
	sink1 := &recSink{}
	if _, err := New().Sync(context.Background(), run(), sink1); err != nil {
		t.Fatalf("run1 Sync: %v", err)
	}
	if fx.sinceSeen {
		t.Fatalf("run1 sent updated_since=%q; the first incremental run must enumerate all", fx.lastSince)
	}
	if sink1.count() != 3 {
		t.Fatalf("run1 emitted %d, want 3", sink1.count())
	}
	if got := state.get(cursorKey("items")); got != "2026-01-03T00:00:00Z" {
		t.Fatalf("run1 stored cursor = %q, want the max updated_at", got)
	}

	// --- Run 2: cursor set -> updated_since sent, only the newer item (added). ---
	fx.mu.Lock()
	fx.items = append(fx.items, map[string]any{"id": "d", "updated_at": "2026-01-04T00:00:00Z"})
	fx.mu.Unlock()

	sink2 := &recSink{}
	if _, err := New().Sync(context.Background(), run(), sink2); err != nil {
		t.Fatalf("run2 Sync: %v", err)
	}
	if !fx.sinceSeen || fx.lastSince != "2026-01-03T00:00:00Z" {
		t.Fatalf("run2 updated_since = %q (seen=%v), want the stored cursor", fx.lastSince, fx.sinceSeen)
	}
	if sink2.count() != 1 {
		t.Fatalf("run2 emitted %d, want 1 (only the newer item)", sink2.count())
	}
	if got := state.get(cursorKey("items")); got != "2026-01-04T00:00:00Z" {
		t.Fatalf("run2 cursor = %q, want it advanced to the new max", got)
	}
}

// TestFullRunIgnoresCursorAndCompletes proves Full==true enumerates everything (no
// updated_since even with a stored cursor) and calls sink.Complete for deletion
// detection, while still advancing the cursor for the next incremental run.
func TestFullRunIgnoresCursorAndCompletes(t *testing.T) {
	restore := syncClient
	t.Cleanup(func() { syncClient = restore })

	fx := &incFixture{items: []map[string]any{
		{"id": "a", "updated_at": "2026-01-01T00:00:00Z"},
		{"id": "b", "updated_at": "2026-01-02T00:00:00Z"},
	}}
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	SetEgressClientForTest(srv.Client())

	state := newMemState()
	// Pre-seed a cursor a full run must ignore.
	_ = state.Set(context.Background(), cursorKey("items"), "2026-06-01T00:00:00Z")

	sink := &recSink{}
	_, err := New().Sync(context.Background(), connector.SyncRun{
		SourceID: uuid.New(), Config: incConfig(t, srv.URL),
		Creds: connector.Credentials(credsFor("bearer")), State: state, Full: true, Log: apiTestLogger(),
	}, sink)
	if err != nil {
		t.Fatalf("full Sync: %v", err)
	}
	if fx.sinceSeen {
		t.Fatalf("full run sent updated_since=%q; a full run must enumerate everything", fx.lastSince)
	}
	if sink.count() != 2 {
		t.Fatalf("full run emitted %d, want 2 (all items)", sink.count())
	}
	if sink.completes != 1 {
		t.Fatalf("full run called Complete %d times, want 1 (deletion detection)", sink.completes)
	}
	if got := state.get(cursorKey("items")); got != "2026-01-02T00:00:00Z" {
		t.Fatalf("full run cursor = %q, want advanced to the max seen", got)
	}
	if got := state.get(stateLastFullSyncKey); got == "" {
		t.Fatalf("full run did not record %s", stateLastFullSyncKey)
	}
}

// TestIncrementalNilStateStillEnumerates proves a nil State (a caller that supplies
// none) degrades to a full enumeration rather than panicking.
func TestIncrementalNilStateStillEnumerates(t *testing.T) {
	restore := syncClient
	t.Cleanup(func() { syncClient = restore })

	fx := &incFixture{items: []map[string]any{{"id": "a", "updated_at": "2026-01-01T00:00:00Z"}}}
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	SetEgressClientForTest(srv.Client())

	sink := &recSink{}
	_, err := New().Sync(context.Background(), connector.SyncRun{
		SourceID: uuid.New(), Config: incConfig(t, srv.URL),
		Creds: connector.Credentials(credsFor("bearer")), State: nil, Full: false, Log: apiTestLogger(),
	}, sink)
	if err != nil {
		t.Fatalf("nil-state Sync: %v", err)
	}
	if sink.count() != 1 {
		t.Fatalf("nil-state emitted %d, want 1", sink.count())
	}
}
