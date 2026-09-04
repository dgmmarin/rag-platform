// Package api is the generic HTTP API source connector (SPEC-04 §4, FR-SRC-07): it
// fetches a tenant-configured JSON API, authenticates each request, walks the
// endpoint's pagination, and streams each record into the ingestion Sink as a
// connector.Document.
//
// Registration happens in this package's init(), so a blank import
// (`_ ".../internal/connector/api"`) at the composition root wires it with no other
// change (NFR-MNT-01), exactly like the upload and web_crawl/sitemap connectors.
//
// STORY BOUNDARY (07.6 vs 07.7, ADR-0048). This story (07.6) builds the fetch/auth/
// pagination/rate-limit engine. Each enumerated item is emitted as a PLACEHOLDER
// Document — the raw item JSON as the body/text, id_path→ExternalID — which makes the
// pagination testable end to end. STORY-07.7 replaces the placeholder mapping with
// text/template rendering (template + helpers), uri_template, metadata JSONPath
// extraction (updated_path/metadata) and incremental sync (incremental_param + a
// cursor persisted in SyncRun.State, weekly full sync). The single seam 07.7 plugs
// into is buildDocument (below); everything else (auth.go, paginate.go, jsonpath.go,
// egress.go) is shared and complete. The config schema already accepts the 07.7
// fields so a full source config validates today.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/rag-platform/ragctl/internal/connector"
)

// Credential keys the connector reads from the decrypted connector.Credentials map
// (SPEC-04 §6, STORY-06.2). Secrets NEVER come from config and are never logged.
const (
	credKeyAPIKey       = "api_key"
	credKeyToken        = "token"
	credKeyUsername     = "username"
	credKeyPassword     = "password"
	credKeyClientID     = "client_id"
	credKeyClientSecret = "client_secret"
)

// apiConfig is the decoded HTTP API connector config (SPEC-04 §4).
type apiConfig struct {
	BaseURL   string     `json:"base_url"`
	Auth      authConfig `json:"auth"`
	Endpoints []endpoint `json:"endpoints"`
}

// authConfig is the NON-SECRET auth shape (which type, which header name, which token
// endpoint). The secret values live in connector.Credentials, not here.
type authConfig struct {
	Type     string   `json:"type"`      // api_key_header | bearer | basic | oauth2_cc
	Header   string   `json:"header"`    // api_key_header: the header name (default X-API-Key)
	TokenURL string   `json:"token_url"` // oauth2_cc: the token endpoint
	Scopes   []string `json:"scopes"`    // oauth2_cc: optional scopes
}

// endpoint is one collection to enumerate. The 07.7 mapping fields (IDPath is used
// as a placeholder in 07.6; UpdatedPath/IncrementalParam/Template/URITemplate/
// Metadata are validated but unused until 07.7).
type endpoint struct {
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	Method     string     `json:"method"`
	Pagination pagination `json:"pagination"`
	ItemsPath  string     `json:"items_path"`
	IDPath     string     `json:"id_path"`
	// --- STORY-07.7 mapping/incremental fields (validated, not used in 07.6) ---
	UpdatedPath      string            `json:"updated_path"`
	IncrementalParam string            `json:"incremental_param"`
	Template         string            `json:"template"`
	URITemplate      string            `json:"uri_template"`
	Metadata         map[string]string `json:"metadata"`
}

// pagination describes how to walk an endpoint's pages (SPEC-04 §4).
type pagination struct {
	Type string `json:"type"` // none | page | offset | cursor | link-header
	// page
	PageParam string `json:"page_param"`
	SizeParam string `json:"size_param"`
	StartPage int    `json:"start_page"`
	// offset
	OffsetParam string `json:"offset_param"`
	LimitParam  string `json:"limit_param"`
	// page + offset share Size (page size / limit)
	Size int `json:"size"`
	// cursor
	CursorParam string `json:"cursor_param"`
	CursorPath  string `json:"cursor_path"`
	// safety ceiling for every type (ponytail: a broken API that never signals the end)
	MaxPages int `json:"max_pages"`
}

// configSchema is the SPEC-04 §4 API config contract. Unknown keys are rejected so a
// typo fails loudly. Semantic checks the schema cannot express (base_url scheme,
// oauth2 needs token_url) are in ValidateConfig. The 07.7 mapping fields are present
// and optional so a full source config validates today.
var configSchema = connector.MustSchemaValidator([]byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["base_url", "auth", "endpoints"],
  "properties": {
    "base_url": {"type": "string", "minLength": 1},
    "auth": {
      "type": "object",
      "additionalProperties": false,
      "required": ["type"],
      "properties": {
        "type": {"enum": ["api_key_header", "bearer", "basic", "oauth2_cc"]},
        "header": {"type": "string"},
        "token_url": {"type": "string"},
        "scopes": {"type": "array", "items": {"type": "string"}}
      }
    },
    "endpoints": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "path", "pagination", "items_path"],
        "properties": {
          "name": {"type": "string", "minLength": 1},
          "path": {"type": "string", "minLength": 1},
          "method": {"type": "string"},
          "items_path": {"type": "string"},
          "id_path": {"type": "string"},
          "updated_path": {"type": "string"},
          "incremental_param": {"type": "string"},
          "template": {"type": "string"},
          "uri_template": {"type": "string"},
          "metadata": {"type": "object", "additionalProperties": {"type": "string"}},
          "pagination": {
            "type": "object",
            "additionalProperties": false,
            "required": ["type"],
            "properties": {
              "type": {"enum": ["none", "page", "offset", "cursor", "link-header"]},
              "page_param": {"type": "string"},
              "size_param": {"type": "string"},
              "start_page": {"type": "integer", "minimum": 0},
              "offset_param": {"type": "string"},
              "limit_param": {"type": "string"},
              "size": {"type": "integer", "minimum": 0},
              "cursor_param": {"type": "string"},
              "cursor_path": {"type": "string"},
              "max_pages": {"type": "integer", "minimum": 0}
            }
          }
        }
      }
    }
  }
}`))

// apiConnector implements connector.Connector for KindAPI.
type apiConnector struct{}

// New returns a fresh API connector.
func New() connector.Connector { return apiConnector{} }

func (apiConnector) Kind() connector.Kind { return connector.KindAPI }

// ValidateConfig checks the config against the JSON schema and the semantic rules the
// schema cannot express.
func (apiConnector) ValidateConfig(cfg json.RawMessage) error {
	if err := configSchema.Validate(cfg); err != nil {
		return err
	}
	var c apiConfig
	if err := json.Unmarshal(cfg, &c); err != nil {
		return &connector.ConfigError{Fields: []connector.FieldError{{Message: "config must be valid JSON"}}}
	}
	return validateSemantics(c)
}

func validateSemantics(c apiConfig) error {
	var fields []connector.FieldError
	if u, err := url.Parse(c.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fields = append(fields, connector.FieldError{Field: "base_url", Message: "must be an absolute http(s) URL"})
	}
	if c.Auth.Type == "oauth2_cc" {
		if u, err := url.Parse(c.Auth.TokenURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fields = append(fields, connector.FieldError{Field: "auth.token_url", Message: "oauth2_cc requires an absolute http(s) token_url"})
		}
	}
	for i, ep := range c.Endpoints {
		if ep.Pagination.Type == "cursor" && strings.TrimSpace(ep.Pagination.CursorPath) == "" {
			fields = append(fields, connector.FieldError{Field: fmt.Sprintf("endpoints.%d.pagination.cursor_path", i), Message: "cursor pagination requires cursor_path"})
		}
	}
	if len(fields) > 0 {
		return &connector.ConfigError{Fields: fields}
	}
	return nil
}

// Test validates the configuration (FR-SRC-14). Live reachability/credential probing
// across all connector kinds is STORY-07.8; here Test guarantees the config is
// well-formed without performing network I/O.
func (apiConnector) Test(_ context.Context, cfg json.RawMessage, _ connector.Credentials) error {
	return apiConnector{}.ValidateConfig(cfg)
}

// Sync enumerates every configured endpoint into the sink (SPEC-04 §4). It builds the
// authenticated, SSRF-guarded HTTP client once, then walks each endpoint's pages.
func (apiConnector) Sync(ctx context.Context, run connector.SyncRun, sink connector.Sink) (connector.Stats, error) {
	var c apiConfig
	if err := json.Unmarshal(run.Config, &c); err != nil {
		return connector.Stats{}, fmt.Errorf("api: decode config: %w", err)
	}
	if err := validateSemantics(c); err != nil {
		return connector.Stats{}, err
	}

	ac, err := buildAuthedClient(ctx, syncClient, c.Auth, run.Creds)
	if err != nil {
		return connector.Stats{}, err // sanitised: buildAuthedClient never embeds secret values
	}

	cl := newClient(c.BaseURL, ac, run.Limiter, run.Log)

	var stats connector.Stats
	for _, ep := range c.Endpoints {
		seq := 0
		emit := func(item any) error {
			doc := buildDocument(ep, item, seq)
			seq++
			changed, err := sink.Put(ctx, doc)
			if err != nil {
				return err
			}
			stats.DocsSeen++
			if changed {
				stats.DocsChanged++
			}
			return nil
		}
		n, err := cl.enumerate(ctx, ep, emit)
		stats.BytesFetched += n
		if err != nil {
			return stats, fmt.Errorf("api: enumerate endpoint %q: %w", ep.Name, err)
		}
	}

	// Full enumeration finished: let the sink reconcile deletions (SPEC-04 §1). The
	// sink itself no-ops Complete on an incremental run (SPEC-05 §5).
	if err := sink.Complete(ctx); err != nil {
		return stats, fmt.Errorf("api: sink complete: %w", err)
	}
	return stats, nil
}

// buildDocument is the SEAM STORY-07.7 replaces. In 07.6 it emits a placeholder
// Document: the raw item JSON as the body text, ExternalID from id_path (namespaced
// by endpoint), so pagination is testable end to end. 07.7 renders template →
// Document.Text, uri_template → URI, metadata JSONPath → Metadata, updated_path →
// ModifiedAt.
func buildDocument(ep endpoint, item any, seq int) connector.Document {
	raw, _ := json.Marshal(item)
	id, ok := evalString(item, ep.IDPath)
	if !ok || id == "" {
		id = fmt.Sprintf("%d", seq)
	}
	return connector.Document{
		ExternalID: ep.Name + "/" + id,
		MimeType:   "application/json",
		Text:       string(raw),
		RawJSON:    json.RawMessage(raw),
	}
}

func init() {
	connector.Register(connector.KindAPI, New)
}
