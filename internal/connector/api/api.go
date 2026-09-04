// Package api is the generic HTTP API source connector (SPEC-04 §4, FR-SRC-07): it
// fetches a tenant-configured JSON API, authenticates each request, walks the
// endpoint's pagination, and streams each record into the ingestion Sink as a
// connector.Document.
//
// Registration happens in this package's init(), so a blank import
// (`_ ".../internal/connector/api"`) at the composition root wires it with no other
// change (NFR-MNT-01), exactly like the upload and web_crawl/sitemap connectors.
//
// STORY BOUNDARY (07.6 vs 07.7). STORY-07.6 (ADR-0048) built the fetch/auth/
// pagination/rate-limit engine (auth.go, paginate.go, jsonpath.go, egress.go).
// STORY-07.7 (ADR-0049) adds the per-item MAPPING — text/template rendering with
// helpers, uri_template, metadata JSONPath extraction, updated_path→ModifiedAt
// (mapping.go) — and INCREMENTAL sync — incremental_param + a cursor persisted in
// SyncRun.State (statestore.go / the tenant connector_state table). The weekly full
// sync that drives deletion detection is set by the EPIC-09 scheduler (SyncRun.Full);
// this connector supports both modes and records last_full_sync for it (ADR-0049).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/rag-platform/ragctl/internal/connector"
)

// State keys the connector persists in SyncRun.State (SPEC-04 §1/§4, ADR-0049). The
// incremental cursor is per endpoint (one source may enumerate several); the
// last-full-sync marker is per source. Keys are namespaced so the store is safe to
// share with any other per-source scratch a future connector adds.
const (
	stateCursorPrefix    = "api:cursor:"
	stateLastFullSyncKey = "api:last_full_sync"
)

// cursorKey is the per-endpoint incremental-cursor State key.
func cursorKey(endpointName string) string { return stateCursorPrefix + endpointName }

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

// endpoint is one collection to enumerate. ItemsPath/IDPath/pagination drive the
// engine (07.6); the mapping/incremental fields drive the per-item Document mapping
// and incremental fetch (07.7, mapping.go).
type endpoint struct {
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	Method     string     `json:"method"`
	Pagination pagination `json:"pagination"`
	ItemsPath  string     `json:"items_path"`
	IDPath     string     `json:"id_path"`
	// --- STORY-07.7 mapping/incremental fields ---
	UpdatedPath      string            `json:"updated_path"`      // → Document.ModifiedAt + incremental cursor
	IncrementalParam string            `json:"incremental_param"` // updated-since query param name
	Template         string            `json:"template"`          // text/template → Document.Text
	URITemplate      string            `json:"uri_template"`      // text/template → Document.URI
	Metadata         map[string]string `json:"metadata"`          // key → JSONPath → Document.Metadata[key]
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
		// A malformed template/uri_template is a config error, caught here so
		// ValidateConfig/Test reject it rather than every item failing at sync time
		// (STORY-07.7, ADR-0049).
		if _, err := newDocMapper(ep); err != nil {
			fields = append(fields, connector.FieldError{Field: fmt.Sprintf("endpoints.%d.template", i), Message: "template does not parse"})
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

	log := run.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	var stats connector.Stats
	for _, ep := range c.Endpoints {
		mapper, err := newDocMapper(ep)
		if err != nil {
			return stats, err // template parse error: a config problem, fail loud
		}

		// Incremental fetch (FR-SRC-08, SPEC-04 §4): on a NON-full run with an
		// incremental_param configured and a stored cursor, pass the cursor as the
		// updated-since query value so the upstream returns only newer records. A full
		// run (or a first run with no cursor) enumerates everything.
		fetch := ep
		if !run.Full && strings.TrimSpace(ep.IncrementalParam) != "" && run.State != nil {
			cur, ok, err := run.State.Get(ctx, cursorKey(ep.Name))
			if err != nil {
				return stats, fmt.Errorf("api: read cursor for endpoint %q: %w", ep.Name, err)
			}
			if ok && cur != "" {
				fetch = withIncrementalParam(ep, ep.IncrementalParam, cur)
			}
		}

		var maxRaw string
		var maxTime time.Time
		seq := 0
		emit := func(item any) error {
			stats.DocsSeen++
			res, berr := mapper.build(item, seq)
			seq++
			if berr != nil {
				// Record and skip one malformed item; never abort the whole sync. The
				// item's content is not logged (SPEC-10: no content at info level).
				log.Warn("api: skipping item after template error", "endpoint", ep.Name, "seq", seq-1)
				return nil
			}
			changed, err := sink.Put(ctx, res.doc)
			if err != nil {
				return err
			}
			if changed {
				stats.DocsChanged++
			}
			if res.hasUpdated && res.updatedTime.After(maxTime) {
				maxTime = res.updatedTime
				maxRaw = res.updatedRaw
			}
			return nil
		}

		n, err := cl.enumerate(ctx, fetch, emit)
		stats.BytesFetched += n
		if err != nil {
			return stats, fmt.Errorf("api: enumerate endpoint %q: %w", ep.Name, err)
		}

		// After a successful enumeration, advance the cursor to the max updated_at seen
		// so the next incremental run resumes from here (on both full and incremental
		// runs, whenever updated_path is configured and we saw a timestamp).
		if run.State != nil && strings.TrimSpace(ep.UpdatedPath) != "" && maxRaw != "" {
			if err := run.State.Set(ctx, cursorKey(ep.Name), maxRaw); err != nil {
				return stats, fmt.Errorf("api: persist cursor for endpoint %q: %w", ep.Name, err)
			}
		}
	}

	// Record when the last full enumeration happened so the EPIC-09 scheduler can
	// decide when the next weekly full sync is due (SPEC-04 §4). Best-effort: a
	// failed breadcrumb must not fail the sync.
	if run.Full && run.State != nil {
		if err := run.State.Set(ctx, stateLastFullSyncKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
			log.Warn("api: could not record last_full_sync", "err", err)
		}
	}

	// Full enumeration finished: let the sink reconcile deletions (SPEC-04 §1). The
	// sink itself no-ops Complete on an incremental run (SPEC-05 §5). The weekly
	// cadence that sets run.Full (and the matching full-mode sink) is the EPIC-09
	// scheduler's job, not the connector's (ADR-0049).
	if err := sink.Complete(ctx); err != nil {
		return stats, fmt.Errorf("api: sink complete: %w", err)
	}
	return stats, nil
}

// withIncrementalParam returns a copy of ep whose Path carries an extra query param
// (the incremental updated-since cursor). It merges into any query already on Path
// and leaves the pagination engine (paginate.go) untouched: withQuery there
// preserves existing query params on every page request, so the cursor rides along
// on all pages of an incremental fetch.
func withIncrementalParam(ep endpoint, param, value string) endpoint {
	u, err := url.Parse(ep.Path)
	if err != nil {
		return ep // a malformed path fails later in enumerate; don't mask it here
	}
	q := u.Query()
	q.Set(param, value)
	u.RawQuery = q.Encode()
	out := ep
	out.Path = u.String()
	return out
}

func init() {
	connector.Register(connector.KindAPI, New)
}
