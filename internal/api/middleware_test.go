package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Recover must turn a panic in an inner handler into a 500 error envelope, never
// leaking the panic value or a stack trace to the client (fail closed).
func TestRecoverConvertsPanicTo500Envelope(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret internal detail")
	})
	h := Recover(log)(panicking)

	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/boom", nil)
	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != CodeInternal {
		t.Fatalf("code = %q, want internal", body.Error.Code)
	}
	if body.Error.Message == "secret internal detail" {
		t.Fatalf("recovery leaked the panic value to the client")
	}
}

// Recover must not swallow a normal (non-panicking) response.
func TestRecoverPassesThroughNormalResponse(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("brew"))
	})
	h := Recover(log)(ok)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rr.Code)
	}
	if rr.Body.String() != "brew" {
		t.Fatalf("body = %q, want brew", rr.Body.String())
	}
}

// chain composes middleware outer-to-inner so the first argument is the
// outermost wrapper (runs first). The test proves order by recording entry.
func TestChainAppliesOuterToInner(t *testing.T) {
	var order []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	final := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	})
	h := chain(final, mw("first"), mw("second"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"first", "second", "handler"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
