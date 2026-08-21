package obs

import (
	"encoding/json"
	"errors"
	"net/http"
)

// errNotReady is a generic not-ready sentinel for tests and simple probes.
var errNotReady = errors.New("not ready")

// Check is a named readiness probe. Later stories register concrete checks —
// control-plane DB reachable, River client started, an embedding provider ping
// (SPEC-10 §4) — without changing this package.
type Check struct {
	Name  string
	Probe func(*http.Request) error
}

// HealthzHandler reports liveness: the process is up. It is always 200 once the
// server is serving (SPEC-10 §4).
func HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
}

// readyzBody is the JSON readiness shape. `checks` is always present (the seam
// later stories fill); `ready` is the aggregate.
type readyzBody struct {
	Ready  bool              `json:"ready"`
	Checks map[string]string `json:"checks"`
}

// ReadyzHandler reports readiness by running the given checks. For this
// scaffolding story the set is typically empty, yielding a ready 200 with a real
// (empty) checks object. A failing check flips the aggregate to 503 and its
// error is reported under its name (SPEC-10 §4).
func ReadyzHandler(checks ...Check) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readyzBody{Ready: true, Checks: map[string]string{}}
		for _, c := range checks {
			if err := c.Probe(r); err != nil {
				body.Ready = false
				body.Checks[c.Name] = err.Error()
			} else {
				body.Checks[c.Name] = "ok"
			}
		}

		code := http.StatusOK
		if !body.Ready {
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
}
