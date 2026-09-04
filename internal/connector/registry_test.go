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
	// Uses the default registry; a kind unique to this test avoids cross-test
	// duplicate-registration panics.
	Register(KindSitemap, func() Connector { return fakeConnector{kind: KindSitemap} })
	c, ok := Lookup(KindSitemap)
	if !ok || c.Kind() != KindSitemap {
		t.Fatalf("package-level lookup failed: ok=%v", ok)
	}
}
