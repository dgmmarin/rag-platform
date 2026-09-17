package eval

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseLimit(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"25", 25, false},
		{"0", 0, false},
		{"-1", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parseLimit(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseLimit(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseLimit(%q) = %d, %v; want %d, nil", c.in, got, err, c.want)
		}
	}
}

// writeServiceError maps the domain sentinels to the public status codes the admin
// UI relies on: a missing run is 404, an unavailable tenant is 503, anything else
// is a generic 500 (never leaking the underlying error).
func TestWriteServiceErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
		code string
	}{
		{ErrNotFound, http.StatusNotFound, "not_found"},
		{ErrTenantUnavailable, http.StatusServiceUnavailable, "tenant_unavailable"},
		{errors.New("boom"), http.StatusInternalServerError, "internal"},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		writeServiceError(rr, c.err, "fallback")
		if rr.Code != c.want {
			t.Errorf("writeServiceError(%v) = %d, want %d", c.err, rr.Code, c.want)
		}
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Error.Code != c.code {
			t.Errorf("code = %q, want %q", body.Error.Code, c.code)
		}
	}
}

// A report request with no tenant in context (the middleware failed to set it) is a
// 401, never a nil-pointer panic reaching the service.
func TestReportNoTenant(t *testing.T) {
	h := NewHandlers(&Service{})
	rr := httptest.NewRecorder()
	h.Report(rr, httptest.NewRequest(http.MethodGet, "/admin/tenants/t-1/eval/runs/r-1", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("Report without tenant = %d, want 401", rr.Code)
	}
}
