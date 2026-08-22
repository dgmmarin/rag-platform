package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The error responses the router-mounted auth middleware/handlers emit must match
// the SPEC-07 §1 envelope shape: {"error":{"code":"...","message":"..."}}. A
// missing-session RequireSession 401 exercises the shared writeError helper.
// STORY-04.1 unifies every router-facing error on this shape (ADR-0027).
func TestErrorEnvelopeMatchesSpec07(t *testing.T) {
	h := (&Handlers{}).RequireSession(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("inner handler must not run for an unauthenticated request")
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/whatever", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not the SPEC-07 object envelope: %v; got %s", err, rr.Body.String())
	}
	if body.Error.Code != "unauthorized" {
		t.Fatalf("error.code = %q, want unauthorized", body.Error.Code)
	}
	if body.Error.Message == "" {
		t.Fatal("error.message is empty")
	}
}
