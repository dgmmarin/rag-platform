package obs

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a private Prometheus registry and the platform's metric
// catalogue (SPEC-10 §2). The registry is private (not the global default) so
// tests and multiple server instances do not fight over one registry.
//
// Cardinality guard (SPEC-10 §2): the tenant label is a low-cardinality tenant
// identifier (one series per tenant), never a client-supplied or unbounded
// value; unresolved requests carry "-". Above ~500 tenants, per-tenant series
// move to the control plane only.
//
// The emission methods are nil-safe: a subsystem wired without a Metrics (its
// optional dependency) calls them on a nil *Metrics as no-ops, so metrics stay
// an optional concern that never changes behaviour.
type Metrics struct {
	registry *prometheus.Registry

	// API plane.
	requestDuration *prometheus.HistogramVec // api_request_duration_seconds {route,status,tenant}
	rateLimited     prometheus.Counter       // api_rate_limited_total

	// Retrieval / answering plane.
	retrievalDuration *prometheus.HistogramVec // query_retrieval_duration_seconds {tenant,reranked}
	queryGrounded     *prometheus.CounterVec   // query_grounded_total {tenant,grounded}

	// Ingestion plane.
	ingestDocuments *prometheus.CounterVec // ingest_documents_total {tenant,source_kind,result}
	ingestChunks    *prometheus.CounterVec // ingest_chunks_total {tenant,provider}
	embedTokens     *prometheus.CounterVec // embed_tokens_total {tenant,provider}

	// Provider plane (LLM / embedding / rerank clients).
	providerDuration *prometheus.HistogramVec // provider_request_duration_seconds {provider,op,status}
	providerErrors   *prometheus.CounterVec   // provider_errors_total {provider,op,status}

	// Jobs plane.
	jobsQueueDepth *prometheus.GaugeVec     // jobs_queue_depth {queue}
	jobsDuration   *prometheus.HistogramVec // jobs_duration_seconds {kind}
	jobsFailed     *prometheus.CounterVec   // jobs_failed_total {kind}
}

// NewMetrics builds a Metrics with its own registry and registers the catalogue.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "api_request_duration_seconds",
			Help:    "Duration of HTTP API requests in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "status", "tenant"}),
		rateLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "api_rate_limited_total",
			Help: "Requests refused with 429 by the rate limiter.",
		}),
		retrievalDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "query_retrieval_duration_seconds",
			Help:    "Duration of hybrid retrieval (and optional rerank) in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"tenant", "reranked"}),
		queryGrounded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "query_grounded_total",
			Help: "Queries by grounding outcome (true = answered from context, false = refused).",
		}, []string{"tenant", "grounded"}),
		ingestDocuments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ingest_documents_total",
			Help: "Documents ingested, by source kind and result (changed/unchanged/failed).",
		}, []string{"tenant", "source_kind", "result"}),
		ingestChunks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ingest_chunks_total",
			Help: "Chunks written during ingestion, per embedding provider.",
		}, []string{"tenant", "provider"}),
		embedTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "embed_tokens_total",
			Help: "Embedding tokens consumed during ingestion, per provider.",
		}, []string{"tenant", "provider"}),
		providerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "provider_request_duration_seconds",
			Help:    "Duration of external provider requests in seconds, by provider/op/status.",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider", "op", "status"}),
		providerErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "provider_errors_total",
			Help: "Failed external provider requests, by provider/op/status.",
		}, []string{"provider", "op", "status"}),
		jobsQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jobs_queue_depth",
			Help: "Jobs waiting to be worked, per queue.",
		}, []string{"queue"}),
		jobsDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "jobs_duration_seconds",
			Help:    "Duration of worked jobs in seconds, per kind.",
			Buckets: prometheus.DefBuckets,
		}, []string{"kind"}),
		jobsFailed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jobs_failed_total",
			Help: "Jobs that ended in a terminal failure, per kind.",
		}, []string{"kind"}),
	}
	reg.MustRegister(
		m.requestDuration,
		m.rateLimited,
		m.retrievalDuration,
		m.queryGrounded,
		m.ingestDocuments,
		m.ingestChunks,
		m.embedTokens,
		m.providerDuration,
		m.providerErrors,
		m.jobsQueueDepth,
		m.jobsDuration,
		m.jobsFailed,
	)
	return m
}

// ObserveRequest records one request's duration under the route/status/tenant
// labels. status is the numeric HTTP status; tenant is the resolved identifier
// (or "-").
func (m *Metrics) ObserveRequest(route string, status int, tenant string, seconds float64) {
	if m == nil {
		return
	}
	m.requestDuration.WithLabelValues(route, strconv.Itoa(status), tenant).Observe(seconds)
}

// ObserveRetrieval records one retrieval's duration; reranked reports whether
// the reranker ran (SPEC-10 §2).
func (m *Metrics) ObserveRetrieval(tenant string, reranked bool, seconds float64) {
	if m == nil {
		return
	}
	m.retrievalDuration.WithLabelValues(tenant, strconv.FormatBool(reranked)).Observe(seconds)
}

// IncGrounded counts one query by its grounding outcome (SPEC-10 §2/§5: the
// per-tenant grounded rate). grounded=false is a refusal (no LLM call).
func (m *Metrics) IncGrounded(tenant string, grounded bool) {
	if m == nil {
		return
	}
	m.queryGrounded.WithLabelValues(tenant, strconv.FormatBool(grounded)).Inc()
}

// IncIngestDocument counts one ingested document by source kind and result
// (changed/unchanged/failed) — SPEC-10 §2 ingestion throughput. The result is an
// outcome label, never document content (C-3).
func (m *Metrics) IncIngestDocument(tenant, sourceKind, result string) {
	if m == nil {
		return
	}
	m.ingestDocuments.WithLabelValues(tenant, sourceKind, result).Add(1)
}

// AddIngestChunks adds n chunks written under the tenant/provider (SPEC-10 §2).
func (m *Metrics) AddIngestChunks(tenant, provider string, n int) {
	if m == nil || n == 0 {
		return
	}
	m.ingestChunks.WithLabelValues(tenant, provider).Add(float64(n))
}

// AddEmbedTokens adds n embedding tokens consumed under the tenant/provider
// (SPEC-10 §2). It is a count only — never the embedded text (C-3).
func (m *Metrics) AddEmbedTokens(tenant, provider string, n int) {
	if m == nil || n == 0 {
		return
	}
	m.embedTokens.WithLabelValues(tenant, provider).Add(float64(n))
}

// ObserveProvider records one external provider request: its duration under
// provider/op/status (status = ok|error), and, when err is non-nil, one
// provider_errors_total for the same labels (SPEC-10 §2/§5). Labels carry no
// per-request content or secrets (C-3/C-4) — only the provider name and the
// operation the caller names.
func (m *Metrics) ObserveProvider(provider, op string, err error, seconds float64) {
	if m == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	m.providerDuration.WithLabelValues(provider, op, status).Observe(seconds)
	if err != nil {
		m.providerErrors.WithLabelValues(provider, op, status).Inc()
	}
}

// ObserveJob records one worked job's duration under its kind (SPEC-10 §2).
func (m *Metrics) ObserveJob(kind string, seconds float64) {
	if m == nil {
		return
	}
	m.jobsDuration.WithLabelValues(kind).Observe(seconds)
}

// IncJobFailed counts one terminally-failed job under its kind (SPEC-10 §5:
// job failures per kind).
func (m *Metrics) IncJobFailed(kind string) {
	if m == nil {
		return
	}
	m.jobsFailed.WithLabelValues(kind).Inc()
}

// SetQueueDepth records the current depth of a queue (SPEC-10 §2/§5: queue depth
// alert). A sampler refreshes it periodically.
func (m *Metrics) SetQueueDepth(queue string, depth int) {
	if m == nil {
		return
	}
	m.jobsQueueDepth.WithLabelValues(queue).Set(float64(depth))
}

// RateLimitedCounter returns the api_rate_limited_total counter so the rate-limit
// middleware (a control-plane package that must not import obs' registry) can
// increment it through the prometheus.Counter interface it already accepts.
func (m *Metrics) RateLimitedCounter() prometheus.Counter {
	if m == nil {
		return nil
	}
	return m.rateLimited
}

// SetPoolGauge registers tenant_pools_open as a GaugeFunc reading open on every
// scrape (SPEC-10 §2). Wired to the resolver's live pool count at the composition
// root; it is a callback so the gauge never drifts from the cache. Calling it more
// than once is a programming error (a metric may be registered once) and panics,
// matching MustRegister.
func (m *Metrics) SetPoolGauge(open func() int) {
	if m == nil || open == nil {
		return
	}
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "tenant_pools_open",
		Help: "Per-tenant database pools currently held open by the resolver cache.",
	}, func() float64 { return float64(open()) }))
}

// Handler returns the Prometheus text exposition handler over this registry, for
// wiring at /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
