package connector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// errUnavailable is the sentinel the control-plane sources package supplies for
// "test connection cannot run because no connector implements this kind yet".
var errUnavailable = errors.New("connector framework not available")

func newValidatorReg(t *testing.T, testCalls *int, testErr error) *Registry {
	t.Helper()
	reg := NewRegistry()
	reg.Register(KindWebCrawl, func() Connector {
		return fakeConnector{
			kind:      KindWebCrawl,
			schema:    MustSchemaValidator([]byte(testSchema)),
			testErr:   testErr,
			testCalls: testCalls,
		}
	})
	return reg
}

func TestSourcesValidatorValidateConfigDelegatesToConnector(t *testing.T) {
	v := NewSourcesValidator(newValidatorReg(t, nil, nil), errUnavailable)
	// A config missing the required start_urls must be rejected by the connector's
	// schema (proving the kind-specific ValidateConfig is wired, NFR-MNT-01).
	if err := v.ValidateConfig(string(KindWebCrawl), json.RawMessage(`{"max_depth":1}`)); err == nil {
		t.Fatal("invalid web_crawl config accepted; connector schema not wired")
	}
	if err := v.ValidateConfig(string(KindWebCrawl), json.RawMessage(`{"start_urls":["https://x/"]}`)); err != nil {
		t.Fatalf("valid web_crawl config rejected: %v", err)
	}
}

func TestSourcesValidatorValidateConfigDefersUnregisteredKind(t *testing.T) {
	// No connector is registered for `api`; kind-specific validation is deferred
	// (generic validation in the sources package still applies), so this is nil —
	// creation of a source whose connector is not built yet must not be blocked.
	v := NewSourcesValidator(NewRegistry(), errUnavailable)
	if err := v.ValidateConfig(string(KindAPI), json.RawMessage(`{"anything":true}`)); err != nil {
		t.Fatalf("unregistered kind should defer (nil), got %v", err)
	}
}

func TestSourcesValidatorTestDelegatesToConnector(t *testing.T) {
	calls := 0
	v := NewSourcesValidator(newValidatorReg(t, &calls, nil), errUnavailable)
	if err := v.Test(context.Background(), string(KindWebCrawl), json.RawMessage(`{"start_urls":["x"]}`), nil); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if calls != 1 {
		t.Fatalf("connector.Test called %d times, want 1", calls)
	}
}

// TestSourcesValidatorTestForwardsCredentials proves the decrypted credentials the
// sources package passes in reach the connector's Test as connector.Credentials
// (STORY-06.2, SPEC-04 §6).
func TestSourcesValidatorTestForwardsCredentials(t *testing.T) {
	var got Credentials
	reg := NewRegistry()
	reg.Register(KindWebCrawl, func() Connector {
		return fakeConnector{kind: KindWebCrawl, gotCreds: &got}
	})
	v := NewSourcesValidator(reg, errUnavailable)
	if err := v.Test(context.Background(), string(KindWebCrawl), json.RawMessage(`{}`), map[string]string{"token": "abc"}); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if got["token"] != "abc" {
		t.Fatalf("connector did not receive credentials: %+v", got)
	}
}

func TestSourcesValidatorTestPropagatesConnectorError(t *testing.T) {
	sentinel := errors.New("unreachable host")
	v := NewSourcesValidator(newValidatorReg(t, nil, sentinel), errUnavailable)
	err := v.Test(context.Background(), string(KindWebCrawl), json.RawMessage(`{"start_urls":["x"]}`), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("want connector error propagated, got %v", err)
	}
}

func TestSourcesValidatorTestUnregisteredKindReturnsUnavailable(t *testing.T) {
	v := NewSourcesValidator(NewRegistry(), errUnavailable)
	err := v.Test(context.Background(), string(KindAPI), json.RawMessage(`{}`), nil)
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("want the injected unavailable sentinel, got %v", err)
	}
}
