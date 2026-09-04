package objectstore

import (
	"context"
	"testing"
)

// New must fail closed on missing connection details BEFORE dialing object
// storage, so a misconfiguration is a startup error rather than a silent
// no-credentials client. The happy-path round trip is covered by the e2e suite
// against the real MinIO stack (a plain unit test has no object store to reach).
func TestNewFailsClosedOnMissingConfig(t *testing.T) {
	cases := map[string]Config{
		"no endpoint":   {Bucket: "b", AccessKey: "a", SecretKey: "s"},
		"no bucket":     {Endpoint: "http://localhost:9000", AccessKey: "a", SecretKey: "s"},
		"no access key": {Endpoint: "http://localhost:9000", Bucket: "b", SecretKey: "s"},
		"no secret key": {Endpoint: "http://localhost:9000", Bucket: "b", AccessKey: "a"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(context.Background(), cfg); err == nil {
				t.Fatalf("New(%+v) = nil error, want a validation error", cfg)
			}
		})
	}
}
