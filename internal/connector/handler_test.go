package connector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandlersListSerializesSchemas (SPEC-11 §10, STORY-11.2): GET
// /admin/connector-kinds serializes the registry's Schemas() into
// { "kinds": [ {kind,label,fields:[{name,label,type,required}]} ] } — field
// descriptors only, never a value (SPEC-04 §6).
func TestHandlersListSerializesSchemas(t *testing.T) {
	reg := NewRegistry()
	reg.Register(KindUpload, func() Connector { return fakeConnector{kind: KindUpload} })
	reg.Register(KindWebCrawl, func() Connector {
		return fakeConnector{kind: KindWebCrawl, fields: []FieldSpec{
			{Name: "start_urls", Label: "Start URLs", Type: "text", Required: true},
		}}
	})
	h := NewHandlers(reg)

	rr := httptest.NewRecorder()
	h.List(rr, httptest.NewRequest(http.MethodGet, "/admin/connector-kinds", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Kinds []struct {
			Kind   string `json:"kind"`
			Label  string `json:"label"`
			Fields []struct {
				Name     string `json:"name"`
				Label    string `json:"label"`
				Type     string `json:"type"`
				Required bool   `json:"required"`
			} `json:"fields"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rr.Body.String())
	}
	if len(body.Kinds) != 2 {
		t.Fatalf("kinds len = %d, want 2: %+v", len(body.Kinds), body.Kinds)
	}
	// sorted: api < ... but here only "upload" and "web_crawl" are registered.
	if body.Kinds[0].Kind != "upload" || body.Kinds[1].Kind != "web_crawl" {
		t.Fatalf("kinds order = %+v, want [upload web_crawl]", body.Kinds)
	}
	if len(body.Kinds[0].Fields) != 0 {
		t.Fatalf("upload fields = %+v, want empty", body.Kinds[0].Fields)
	}
	wc := body.Kinds[1]
	if wc.Label != "Web Crawl" {
		t.Fatalf("web_crawl label = %q, want %q", wc.Label, "Web Crawl")
	}
	if len(wc.Fields) != 1 || wc.Fields[0].Name != "start_urls" || wc.Fields[0].Type != "text" || !wc.Fields[0].Required {
		t.Fatalf("web_crawl fields = %+v", wc.Fields)
	}
}

func TestHandlersListEmptyRegistry(t *testing.T) {
	h := NewHandlers(NewRegistry())
	rr := httptest.NewRecorder()
	h.List(rr, httptest.NewRequest(http.MethodGet, "/admin/connector-kinds", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != `{"kinds":[]}`+"\n" {
		t.Fatalf("body = %q, want %q", rr.Body.String(), `{"kinds":[]}`+"\n")
	}
}
