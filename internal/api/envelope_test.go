package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The error envelope must match SPEC-07 §1 exactly:
//
//	{"error":{"code":"not_found","message":"...","request_id":"..."}}
func TestWriteErrorEnvelopeShape(t *testing.T) {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)

	WriteError(rr, r, http.StatusNotFound, CodeNotFound, "gone")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != CodeNotFound {
		t.Fatalf("code = %q, want %q", body.Error.Code, CodeNotFound)
	}
	if body.Error.Message != "gone" {
		t.Fatalf("message = %q, want gone", body.Error.Message)
	}
}

// The envelope must carry the request id set by the obs middleware so a client
// can correlate an error with server logs (SPEC-07 §1, SPEC-10).
func TestWriteErrorEnvelopeCarriesRequestID(t *testing.T) {
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	r.Header.Set("X-Request-Id", "req-abc")
	// The obs middleware stashes the id in the context; simulate that here.
	r = r.WithContext(requestIDContext(r.Context(), "req-abc"))

	WriteError(rr, r, http.StatusBadRequest, CodeValidation, "bad")

	var body struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.RequestID != "req-abc" {
		t.Fatalf("request_id = %q, want req-abc", body.Error.RequestID)
	}
}

// CodeForStatus maps HTTP statuses to the SPEC-07 §1 error-code vocabulary so a
// generic path (recovery, method-not-allowed) still emits a spec code.
func TestCodeForStatus(t *testing.T) {
	cases := map[int]string{
		http.StatusUnauthorized:        CodeUnauthorized,
		http.StatusForbidden:           CodeForbidden,
		http.StatusNotFound:            CodeNotFound,
		http.StatusBadRequest:          CodeValidation,
		http.StatusTooManyRequests:     CodeRateLimited,
		http.StatusConflict:            CodeConflict,
		http.StatusServiceUnavailable:  CodeTenantUnavailable,
		http.StatusInternalServerError: CodeInternal,
	}
	for status, want := range cases {
		if got := CodeForStatus(status); got != want {
			t.Errorf("CodeForStatus(%d) = %q, want %q", status, got, want)
		}
	}
}
