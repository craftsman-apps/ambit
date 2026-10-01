package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPeriodDefaultsAndValidation(t *testing.T) {
	from, to, err := period(httptest.NewRequest("GET", "/api/audit?from=2026-03-01&to=2026-04-01", nil))
	if err != nil || !from.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("march: %v %v %v", from, to, err)
	}
	from, to, err = period(httptest.NewRequest("GET", "/api/audit", nil))
	if err != nil || to.Sub(from) != 30*24*time.Hour {
		t.Fatalf("default: %v %v %v", from, to, err)
	}
	for _, q := range []url.Values{{"from": {"2026-04-01"}, "to": {"2026-03-01"}}, {"from": {"march"}}, {"to": {"1'; DROP TABLE otel_logs"}}} {
		if _, _, err := period(httptest.NewRequest("GET", "/api/audit?"+q.Encode(), nil)); err == nil {
			t.Errorf("%s accepted", q)
		}
	}
}

func TestArrayLiteralEscapes(t *testing.T) {
	if got := arrayLiteral([]string{"a", `b'c`, `d\e`}); got != `['a','b\'c','d\\e']` {
		t.Fatalf("%s", got)
	}
	if got := arrayLiteral(nil); got != "[]" {
		t.Fatalf("%s", got)
	}
}

// A failing audit store is reported as such, not as a Kubernetes problem (seen in the console review:
// ClickHouse ran out of memory and the page said "could not reach Kubernetes").
func TestAuditStoreFailureNamesClickHouse(t *testing.T) {
	a := OpenAudit(ClickHouse{Addr: "127.0.0.1:1", Password: "x"}) // nothing listens here
	mux := http.NewServeMux()
	a.register(mux, func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h(w, r, Session{Email: "priya@acme.test", Role: RoleAdmin})
		}
	})
	for _, u := range []string{"/api/audit?email=alice%40acme.test", "/api/usage"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", u, nil))
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "ClickHouse") || strings.Contains(rec.Body.String(), "Kubernetes") {
			t.Errorf("%s: %d %s", u, rec.Code, rec.Body.String())
		}
	}
}
