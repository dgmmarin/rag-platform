package obs

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a private Prometheus registry and the platform's HTTP request
// histogram. This story implements the single representative metric,
// api_request_duration_seconds (SPEC-10 §2); STORY-10.1 adds the full catalogue.
//
// Cardinality guard (SPEC-10 §2): the tenant label is the tenant slug, not an
// unbounded id; until STORY-02 resolves tenants the middleware passes "-".
type Metrics struct {
	registry        *prometheus.Registry
	requestDuration *prometheus.HistogramVec
}

// NewMetrics builds a Metrics with its own registry so tests and multiple server
// instances do not fight over the global default registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	requestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "api_request_duration_seconds",
			Help:    "Duration of HTTP API requests in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"route", "status", "tenant"},
	)
	reg.MustRegister(requestDuration)
	return &Metrics{registry: reg, requestDuration: requestDuration}
}

// ObserveRequest records one request's duration under the route/status/tenant
// labels. status is the numeric HTTP status; tenant is the slug (or "-").
func (m *Metrics) ObserveRequest(route string, status int, tenant string, seconds float64) {
	m.requestDuration.WithLabelValues(route, strconv.Itoa(status), tenant).Observe(seconds)
}

// Handler returns the Prometheus text exposition handler over this registry, for
// wiring at /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
