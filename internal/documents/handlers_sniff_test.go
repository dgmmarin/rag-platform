package documents

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIngestRejectsContentTypeMismatch proves the handler sniffs the file's bytes
// rather than trusting the extension/Content-Type: a plain-text body with a .pdf
// name is rejected 400 before it reaches storage (SPEC-04 §5 security).
func TestIngestRejectsContentTypeMismatch(t *testing.T) {
	h := NewHandlers(withStorage(NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{})))
	body, ct := multipartBody(t, "file", "notreally.pdf", "this is just text, not a pdf", nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", body).WithContext(withTenant(t))
	req.Header.Set("Content-Type", ct)
	h.Ingest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("content/extension mismatch = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestIngestEnforcesPerTenantLimit proves the size ceiling comes from the tenant's
// settings (FR-SRC-02): a small settings limit rejects an upload the larger global
// ceiling would allow.
func TestIngestEnforcesPerTenantLimit(t *testing.T) {
	svc := withStorage(NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{}))
	svc.MaxBytes = 1 << 20         // generous global ceiling
	svc.Limits = fakeLimits{mb: 8} // tenant-configured tiny ceiling (bytes)
	h := NewHandlers(svc)
	body, ct := multipartBody(t, "file", "big.txt", strings.Repeat("A", 1024), nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", body).WithContext(withTenant(t))
	req.Header.Set("Content-Type", ct)
	h.Ingest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("per-tenant oversize = %d, want 400", rr.Code)
	}
}
