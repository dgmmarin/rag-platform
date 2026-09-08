package connector

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeConnector is a test double implementing the full SPEC-04 §1 Connector
// interface. Only Kind/ValidateConfig/Test carry behaviour the framework tests
// exercise; Sync is a stub (its implementation is EPIC-07, out of this story).
type fakeConnector struct {
	kind      Kind
	schema    *SchemaValidator
	testErr   error
	testCalls *int
	gotCreds  *Credentials // if set, Test records the credentials it received
	fields    []FieldSpec
}

func (c fakeConnector) Kind() Kind { return c.kind }

func (c fakeConnector) ValidateConfig(cfg json.RawMessage) error {
	if c.schema == nil {
		return nil
	}
	return c.schema.Validate(cfg)
}

func (c fakeConnector) Test(_ context.Context, _ json.RawMessage, creds Credentials) error {
	if c.testCalls != nil {
		*c.testCalls++
	}
	if c.gotCreds != nil {
		*c.gotCreds = creds
	}
	return c.testErr
}

func (c fakeConnector) Sync(_ context.Context, _ SyncRun, _ Sink) (Stats, error) {
	return Stats{}, nil
}

func (c fakeConnector) Fields() []FieldSpec { return c.fields }

func TestRegistryLookupReturnsRegisteredConnector(t *testing.T) {
	reg := NewRegistry()
	reg.Register(KindWebCrawl, func() Connector { return fakeConnector{kind: KindWebCrawl} })

	c, ok := reg.Lookup(KindWebCrawl)
	if !ok {
		t.Fatal("registered kind not found")
	}
	if c.Kind() != KindWebCrawl {
		t.Fatalf("kind = %q, want %q", c.Kind(), KindWebCrawl)
	}
}

func TestRegistryLookupUnknownKind(t *testing.T) {
	reg := NewRegistry()
	if _, ok := reg.Lookup(KindS3); ok {
		t.Fatal("unregistered kind reported found")
	}
}

func TestRegistryLookupReturnsFreshInstance(t *testing.T) {
	reg := NewRegistry()
	n := 0
	reg.Register(KindAPI, func() Connector { n++; return fakeConnector{kind: KindAPI} })
	reg.Lookup(KindAPI)
	reg.Lookup(KindAPI)
	if n != 2 {
		t.Fatalf("factory called %d times, want 2 (one fresh instance per Lookup)", n)
	}
}

func TestRegistryRegisterDuplicatePanics(t *testing.T) {
	reg := NewRegistry()
	reg.Register(KindWebCrawl, func() Connector { return fakeConnector{kind: KindWebCrawl} })
	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate kind should panic")
		}
	}()
	reg.Register(KindWebCrawl, func() Connector { return fakeConnector{kind: KindWebCrawl} })
}

func TestRegistryRegisterNilFactoryPanics(t *testing.T) {
	reg := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("registering a nil factory should panic")
		}
	}()
	reg.Register(KindWebCrawl, nil)
}

func TestRegistryKindsSorted(t *testing.T) {
	reg := NewRegistry()
	reg.Register(KindWebCrawl, func() Connector { return fakeConnector{kind: KindWebCrawl} })
	reg.Register(KindAPI, func() Connector { return fakeConnector{kind: KindAPI} })
	kinds := reg.Kinds()
	if len(kinds) != 2 || kinds[0] != KindAPI || kinds[1] != KindWebCrawl {
		t.Fatalf("kinds = %v, want sorted [api web_crawl]", kinds)
	}
}

func TestPackageLevelRegisterAndLookup(t *testing.T) {
	// Uses the default registry; a kind unique to this test (not one of the real
	// SPEC-04 §1 kinds) avoids a duplicate-registration panic against the real
	// connectors' own init() registrations — the drift-guard test
	// (kinds_test.go) imports those packages into this same test binary.
	const testKind Kind = "test_pkg_level_only"
	Register(testKind, func() Connector { return fakeConnector{kind: testKind} })
	c, ok := Lookup(testKind)
	if !ok || c.Kind() != testKind {
		t.Fatalf("package-level lookup failed: ok=%v", ok)
	}
}

// TestRegistrySchemasListsRegisteredKindsWithFields (SPEC-11 §10, STORY-11.2):
// Schemas() returns one KindSchema per REGISTERED kind, sorted, carrying the
// label and the connector's own Fields() — an unregistered kind (KindS3 here) is
// simply absent, same as Lookup/ValidateConfig already defer for it.
func TestRegistrySchemasListsRegisteredKindsWithFields(t *testing.T) {
	reg := NewRegistry()
	wcFields := []FieldSpec{{Name: "start_urls", Label: "Start URLs", Type: "text", Required: true}}
	apiFields := []FieldSpec{{Name: "base_url", Label: "Base URL", Type: "url", Required: true}}
	reg.Register(KindWebCrawl, func() Connector { return fakeConnector{kind: KindWebCrawl, fields: wcFields} })
	reg.Register(KindAPI, func() Connector { return fakeConnector{kind: KindAPI, fields: apiFields} })

	schemas := reg.Schemas()
	if len(schemas) != 2 {
		t.Fatalf("Schemas() len = %d, want 2: %+v", len(schemas), schemas)
	}
	if schemas[0].Kind != string(KindAPI) || schemas[1].Kind != string(KindWebCrawl) {
		t.Fatalf("Schemas() not sorted by kind: %+v", schemas)
	}
	if schemas[0].Label != "API" {
		t.Fatalf("api label = %q, want %q", schemas[0].Label, "API")
	}
	if len(schemas[0].Fields) != 1 || schemas[0].Fields[0] != apiFields[0] {
		t.Fatalf("api fields = %+v, want %+v", schemas[0].Fields, apiFields)
	}
	if schemas[1].Label != "Web Crawl" {
		t.Fatalf("web_crawl label = %q, want %q", schemas[1].Label, "Web Crawl")
	}
	if len(schemas[1].Fields) != 1 || schemas[1].Fields[0] != wcFields[0] {
		t.Fatalf("web_crawl fields = %+v, want %+v", schemas[1].Fields, wcFields)
	}
}

func TestRegistrySchemasEmptyRegistry(t *testing.T) {
	reg := NewRegistry()
	schemas := reg.Schemas()
	if len(schemas) != 0 {
		t.Fatalf("Schemas() on empty registry = %+v, want empty", schemas)
	}
}
