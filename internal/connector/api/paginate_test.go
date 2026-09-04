package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func apiTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func noAuth(srv *httptest.Server) authClient { return authClient{client: srv.Client()} }

// TestPaginationWalksAllPages proves each of the five pagination types enumerates
// every fixture item (7 across pages of 3), and that a multi-page type actually
// issued more than one request.
func TestPaginationWalksAllPages(t *testing.T) {
	for _, pt := range pagTypes {
		pt := pt
		t.Run(pt, func(t *testing.T) {
			ph := &paginationHandler{pagType: pt}
			srv := httptest.NewServer(ph)
			defer srv.Close()

			c := newClient(srv.URL, noAuth(srv), nil, apiTestLogger())
			ep := endpoint{
				Name: "items", Path: "/items", Method: http.MethodGet,
				Pagination: paginationConfig(pt), ItemsPath: "$.data", IDPath: "$.id",
			}

			var ids []string
			_, err := c.enumerate(context.Background(), ep, func(item any) error {
				id, ok := evalString(item, "$.id")
				if !ok {
					t.Fatalf("item missing id: %v", item)
				}
				ids = append(ids, id)
				return nil
			})
			if err != nil {
				t.Fatalf("enumerate(%s): %v", pt, err)
			}
			if len(ids) != fixtureTotal {
				t.Fatalf("pagination %s enumerated %d items, want %d (%v)", pt, len(ids), fixtureTotal, ids)
			}
			// none is one request; every paginated type must have walked >1 page.
			if pt != "none" && ph.hits() < 3 {
				t.Fatalf("pagination %s made %d requests, want >=3 (did it stop early?)", pt, ph.hits())
			}
			if pt == "none" && ph.hits() != 1 {
				t.Fatalf("pagination none made %d requests, want 1", ph.hits())
			}
		})
	}
}

// TestPaginationMaxPagesCeiling proves the safety ceiling stops a broken API that
// never signals the end (a cursor server that always returns a next cursor).
func TestPaginationMaxPagesCeiling(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"data": []map[string]any{{"id": "x"}}, "next_cursor": "always"})
	}))
	defer srv.Close()

	c := newClient(srv.URL, noAuth(srv), nil, apiTestLogger())
	ep := endpoint{
		Name: "items", Path: "/items", Method: http.MethodGet,
		Pagination: pagination{Type: "cursor", CursorParam: "cursor", CursorPath: "$.next_cursor", MaxPages: 5},
		ItemsPath:  "$.data", IDPath: "$.id",
	}
	n := 0
	if _, err := c.enumerate(context.Background(), ep, func(any) error { n++; return nil }); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if got := int(atomic.LoadInt32(&hits)); got != 5 {
		t.Fatalf("never-ending cursor: %d requests, want exactly max_pages=5", got)
	}
}

// TestRetryAfterParse checks the Retry-After parser for both wire forms without
// sleeping.
func TestRetryAfterParse(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "5")
	if d, ok := retryAfter(h); !ok || d != 5*time.Second {
		t.Fatalf("numeric Retry-After = %v, %v; want 5s", d, ok)
	}

	h.Set("Retry-After", time.Now().Add(2*time.Second).UTC().Format(http.TimeFormat))
	d, ok := retryAfter(h)
	if !ok || d <= 0 || d > 3*time.Second {
		t.Fatalf("date Retry-After = %v, %v; want ~2s", d, ok)
	}

	if _, ok := retryAfter(http.Header{}); ok {
		t.Fatalf("absent Retry-After should be not-ok")
	}
}

// TestRetryAfterRetriesThenSucceeds proves a 429 with Retry-After is honoured: the
// fixture 429s once (Retry-After: 1) then serves the data on the retry.
func TestRetryAfterRetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, map[string]any{"data": allFixtureItems()})
	}))
	defer srv.Close()

	c := newClient(srv.URL, noAuth(srv), nil, apiTestLogger())
	ep := endpoint{Name: "items", Path: "/items", Method: http.MethodGet, Pagination: pagination{Type: "none"}, ItemsPath: "$.data", IDPath: "$.id"}

	start := time.Now()
	n := 0
	if _, err := c.enumerate(context.Background(), ep, func(any) error { n++; return nil }); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if n != fixtureTotal {
		t.Fatalf("after 429+retry enumerated %d, want %d", n, fixtureTotal)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("expected 2 server calls (429 then 200), got %d", calls)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("retry did not honour Retry-After ~1s (elapsed %v)", time.Since(start))
	}
}
