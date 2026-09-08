package auth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rag-platform/ragctl/internal/obs"
)

// TestRequireScopePopulatesRequestTenant proves the scope middleware records the
// key's tenant for the obs request label (SPEC-10 §2): a request wrapped by the
// obs middleware ends up labelled with the resolved tenant, not "-".
func TestRequireScopePopulatesRequestTenant(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	const tid = "22222222-2222-2222-2222-222222222222"
	rows := map[string]keyRow{}
	secret, _ := mintInto(t, rows, tid, "query")
	v, _ := mkVerifier(rows, now)

	m := obs.NewMetrics()
	inner := v.RequireScope(ScopeQuery)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h := obs.Middleware(obs.Logger("t", slog.LevelInfo, &bytes.Buffer{}), m)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/query", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	h.ServeHTTP(httptest.NewRecorder(), req)

	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rr.Body.String(), `tenant="`+tid+`"`) {
		t.Fatalf("request histogram not labelled with resolved tenant:\n%s", rr.Body.String())
	}
}
