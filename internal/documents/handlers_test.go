package documents

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rag-platform/ragctl/internal/tenant"
)

// withTenant returns a request context carrying the resolved tenant identity, as
// the API-key scope middleware would set it (FR-ACC-03).
func withTenant(t *testing.T) context.Context {
	return tenant.WithTenantID(context.Background(), newTID(t))
}

func multipartBody(t *testing.T, field, filename, content string, extra map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		fw, err := mw.CreateFormFile(field, filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		_, _ = fw.Write([]byte(content))
	}
	for k, v := range extra {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestListRequiresTenant(t *testing.T) {
	h := NewHandlers(NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{}))
	rr := httptest.NewRecorder()
	// No tenant in context.
	h.List(rr, httptest.NewRequest(http.MethodGet, "/v1/documents", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("list without tenant = %d, want 401", rr.Code)
	}
}

func TestListBadLimit(t *testing.T) {
	h := NewHandlers(NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/documents?limit=-3", nil).WithContext(withTenant(t))
	h.List(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad limit = %d, want 400", rr.Code)
	}
}

func TestGetForwardsContentQuery(t *testing.T) {
	fs := &fakeStore{getDetail: DocumentDetail{Document: Document{ID: "d1"}}}
	h := NewHandlers(NewService(fakeResolver{}, fs, &fakeJobs{}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/documents/d1?content=true", nil).WithContext(withTenant(t))
	req.SetPathValue("id", "d1")
	h.Get(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200", rr.Code)
	}
	if !fs.gotContent {
		t.Fatal("?content=true not forwarded")
	}
}

func TestGetNotFound(t *testing.T) {
	h := NewHandlers(NewService(fakeResolver{}, &fakeStore{getErr: ErrNotFound}, &fakeJobs{}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/documents/x", nil).WithContext(withTenant(t))
	req.SetPathValue("id", "x")
	h.Get(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get missing = %d, want 404", rr.Code)
	}
	assertCode(t, rr, "not_found")
}

func TestIngestNoFile(t *testing.T) {
	h := NewHandlers(withStorage(NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{})))
	body, ct := multipartBody(t, "file", "", "", map[string]string{"note": "x"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", body).WithContext(withTenant(t))
	req.Header.Set("Content-Type", ct)
	h.Ingest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("no file = %d, want 400", rr.Code)
	}
}

func TestIngestDisallowedType(t *testing.T) {
	h := NewHandlers(withStorage(NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{})))
	body, ct := multipartBody(t, "file", "malware.exe", "MZ", nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", body).WithContext(withTenant(t))
	req.Header.Set("Content-Type", ct)
	h.Ingest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("disallowed type = %d, want 400", rr.Code)
	}
}

func TestIngestOversize(t *testing.T) {
	svc := withStorage(NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{}))
	svc.MaxBytes = 8 // tiny ceiling
	h := NewHandlers(svc)
	body, ct := multipartBody(t, "file", "big.txt", strings.Repeat("A", 1024), nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", body).WithContext(withTenant(t))
	req.Header.Set("Content-Type", ct)
	h.Ingest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversize = %d, want 400", rr.Code)
	}
}

func TestIngestNilStorageSeam(t *testing.T) {
	h := NewHandlers(NewService(fakeResolver{}, &fakeStore{}, &fakeJobs{})) // Storage nil
	body, ct := multipartBody(t, "file", "a.pdf", "%PDF-1.4", nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", body).WithContext(withTenant(t))
	req.Header.Set("Content-Type", ct)
	h.Ingest(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("nil storage = %d, want 404 seam", rr.Code)
	}
	assertCode(t, rr, "not_found")
}

func TestIngestHappyPath(t *testing.T) {
	jobs := &fakeJobs{}
	svc := NewService(fakeResolver{}, &fakeStore{}, jobs)
	svc.Storage = &fakeStorage{}
	h := NewHandlers(svc)
	body, ct := multipartBody(t, "file", "doc.md", "# hi", map[string]string{"source": "33333333-3333-3333-3333-333333333333"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/documents", body).WithContext(withTenant(t))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Idempotency-Key", "k9")
	h.Ingest(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("ingest = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Job Job `json:"job"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Job.Kind != "ingest_document" {
		t.Fatalf("job kind = %q", out.Job.Kind)
	}
	if len(jobs.enqueued) != 1 || jobs.enqueued[0].SourceID == nil {
		t.Fatalf("expected one enqueue with source_id: %+v", jobs.enqueued)
	}
}

func withStorage(s *Service) *Service {
	s.Storage = &fakeStorage{}
	return s
}

func assertCode(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, rr.Body.String())
	}
	if env.Error.Code != want {
		t.Fatalf("error code = %q, want %q", env.Error.Code, want)
	}
}
