//go:build e2e

// STORY-06.2 golden path: source credential encryption and handling (FR-SRC-10,
// SPEC-04 §6) end to end over the REAL public HTTP router (internal/api) served on
// a real net/http listener against the REAL control-plane Postgres (up via
// `mise run up`), with the REAL envelope Cipher (internal/crypto), no mocks. It
// proves through the assembled API-key admin chain (scope admin -> rate limit ->
// handler):
//   - POST /v1/sources with `credentials` is accepted (201) and the response body
//     never echoes the secret (FR-SRC-10),
//   - the stored credentials_enc column is CIPHERTEXT, not the plaintext secret
//     (asserted by reading the row directly over the control-plane pool),
//   - GET /v1/sources/{id} never returns credentials,
//   - POST /v1/sources/{id}/test decrypts the stored credentials and hands the
//     plaintext to the connector's Test (SPEC-04 §6).
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/api"
	"github.com/rag-platform/ragctl/internal/connector"
	"github.com/rag-platform/ragctl/internal/cp/auth"
	"github.com/rag-platform/ragctl/internal/cp/ratelimit"
	"github.com/rag-platform/ragctl/internal/cp/sources"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/crypto"
	"github.com/rag-platform/ragctl/internal/obs"
)

// credsRecordingConnector is a full connector.Connector registered by this e2e so
// a web_crawl source exists; Test records the credentials it received so the test
// can assert the decrypt-and-forward path (SPEC-04 §6).
type credsRecordingConnector struct{ got *connector.Credentials }

func (c credsRecordingConnector) Kind() connector.Kind                   { return connector.KindWebCrawl }
func (c credsRecordingConnector) ValidateConfig(_ json.RawMessage) error { return nil }
func (c credsRecordingConnector) Test(_ context.Context, _ json.RawMessage, creds connector.Credentials) error {
	// Copy: the sources service zeroes the map after Test returns.
	cp := connector.Credentials{}
	for k, v := range creds {
		cp[k] = v
	}
	*c.got = cp
	return nil
}
func (c credsRecordingConnector) Sync(_ context.Context, _ connector.SyncRun, _ connector.Sink) (connector.Stats, error) {
	return connector.Stats{}, nil
}
func (c credsRecordingConnector) Fields() []connector.FieldSpec { return nil }

func TestSourceCredentialsGoldenPath(t *testing.T) {
	migrateControl(t)
	pool := controlPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	suffix := mustSuffix(t)
	slug := "creds-" + suffix

	var tenantID string
	if err := pool.QueryRow(ctx,
		`insert into tenants (slug, name, status, region) values ($1, $2, 'active', 'eu-central') returning id::text`,
		slug, "Credentials Test "+suffix).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		user := hostPort("POSTGRES_USER", "rag")
		_ = tryPsql(user, "control_plane", fmt.Sprintf("DELETE FROM tenants WHERE id = '%s'", tenantID))
	})

	keySvc := auth.NewAPIKeyService(auth.MembershipFromPool(pool))
	_, secret, err := keySvc.Create(ctx, auth.CreateKeyParams{TenantID: tenantID, Name: "admin-token", Scopes: []string{"admin"}})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// --- The REAL envelope Cipher (SPEC-09 §2) with a fresh random DEK. ---
	dek := make([]byte, crypto.KeySize)
	if _, err := rand.Read(dek); err != nil {
		t.Fatalf("dek: %v", err)
	}
	cipher, err := crypto.NewCipher(1, dek)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}

	// --- A registry with a credentials-recording web_crawl connector, wired into
	// the sources service exactly as buildAPIServer wires the default registry. ---
	var recorded connector.Credentials
	reg := connector.NewRegistry()
	reg.Register(connector.KindWebCrawl, func() connector.Connector {
		return credsRecordingConnector{got: &recorded}
	})
	sourcesSvc := sources.NewService(sources.FromPool(pool))
	sourcesSvc.Validator = connector.NewSourcesValidator(reg, sources.ErrConnectorUnavailable)
	sourcesSvc.Encrypter = cipher
	sourcesSvc.Decrypter = cipher

	verifier := auth.NewAPIKeyVerifier(auth.FromPool(pool))
	limiter := ratelimit.New(nil)
	settingsSvc := tenants.NewSettingsService(tenants.SettingsFromPool(pool))
	rl := &ratelimit.Middleware{Limiter: limiter, Limit: ratelimit.LimitFromSettings(settingsSvc, 1000), Burst: 1000, TenantBurst: 1000}
	sh := sources.NewHandlers(sourcesSvc)
	deps := api.Deps{
		Log:               obs.Logger("e2e", 0, bytes.NewBuffer(nil)),
		Metrics:           obs.NewMetrics(),
		RequireScopeAdmin: verifier.RequireScope(auth.ScopeAdmin),
		RateLimit:         rl.Handler,
		SourceCreate:      http.HandlerFunc(sh.Create),
		SourceGet:         http.HandlerFunc(sh.Get),
		SourceTest:        http.HandlerFunc(sh.Test),
	}
	srv := httptest.NewServer(api.New(deps))
	defer srv.Close()

	bearer := "Bearer " + secret
	call := func(method, path, body string) (int, []byte) {
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, r)
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", bearer)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	const secretValue = "tok_live_super_secret_value"

	// --- Create a source WITH credentials: 201, no secret echoed (FR-SRC-10). ---
	body := `{"kind":"web_crawl","name":"creds-` + suffix + `","config":{"start_urls":["https://x/"]},"credentials":{"api_token":"` + secretValue + `"}}`
	code, out := call(http.MethodPost, "/v1/sources", body)
	if code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", code, out)
	}
	if strings.Contains(string(out), secretValue) || strings.Contains(string(out), "credential") {
		t.Fatalf("create response leaks credentials: %s", out)
	}
	var created sources.Source
	if err := json.Unmarshal(out, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// --- The stored column is CIPHERTEXT, not the plaintext secret. ---
	var enc []byte
	if err := pool.QueryRow(ctx, `select credentials_enc from sources where id = $1`, created.ID).Scan(&enc); err != nil {
		t.Fatalf("read credentials_enc: %v", err)
	}
	if len(enc) == 0 {
		t.Fatal("credentials_enc is null; credentials were not stored")
	}
	if bytes.Contains(enc, []byte(secretValue)) {
		t.Fatalf("credentials_enc contains the plaintext secret")
	}
	// It decrypts back to the original JSON with the platform cipher (round-trip).
	pt, err := cipher.Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt stored credentials: %v", err)
	}
	if !bytes.Contains(pt, []byte(secretValue)) {
		t.Fatalf("stored ciphertext does not round-trip to the secret")
	}

	// --- GET never returns credentials (FR-SRC-10). ---
	code, out = call(http.MethodGet, "/v1/sources/"+created.ID, "")
	if code != http.StatusOK {
		t.Fatalf("get = %d, want 200; body=%s", code, out)
	}
	if strings.Contains(string(out), secretValue) || strings.Contains(string(out), "credential") {
		t.Fatalf("get response leaks credentials: %s", out)
	}

	// --- /test decrypts and hands the plaintext to the connector (SPEC-04 §6). ---
	if code, out := call(http.MethodPost, "/v1/sources/"+created.ID+"/test", ""); code != http.StatusOK {
		t.Fatalf("test = %d, want 200; body=%s", code, out)
	}
	if recorded["api_token"] != secretValue {
		t.Fatalf("connector.Test did not receive the decrypted credential, got %+v", recorded)
	}
}
