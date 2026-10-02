package console

import (
	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
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

// The audit reads the status under the v1.5 and the v1.6 attribute names (upgrade check, 2026-10-01).
func TestCallsSQLReadsBothStatusNames(t *testing.T) {
	if !strings.Contains(callsSQL, "LogAttributes['http.status']") || !strings.Contains(callsSQL, "LogAttributes['http.response.status_code']") || strings.Contains(callsSQL, "{status}") {
		t.Fatal(callsSQL)
	}
}

// Usage totals per group and day, with cost from each model's price; tokens of models without a price
// are counted but named, so the cost is never silently low.
func TestUsageCost(t *testing.T) {
	rows := []usageRow{
		{Day: "2026-10-01", Group: "eng", Model: "gpt-small", Requests: 10, In: 1_000_000, Out: 500_000},
		{Day: "2026-10-01", Group: "eng", Model: "llama-local", Requests: 5, In: 2000, Out: 100},
		{Day: "2026-10-01", Group: "eng", Model: "", Requests: 2}, // refused before a model was read
		{Day: "2026-10-01", Group: "ml", Model: "llama-local", Requests: 1, In: 10, Out: 10},
	}
	prices := map[string]v1.ModelPrice{"gpt-small": {InputPerMillionTokens: "0.15", OutputPerMillionTokens: "0.6"}}
	got := aggregateUsage(rows, prices)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	eng := got[0]
	if eng.Group != "eng" || eng.Requests != 17 || eng.InputTokens != 1_002_000 || eng.OutputTokens != 500_100 {
		t.Fatalf("eng %+v", eng)
	}
	if eng.Cost == nil || *eng.Cost < 0.4499 || *eng.Cost > 0.4501 || len(eng.UnpricedModels) != 1 || eng.UnpricedModels[0] != "llama-local" {
		t.Fatalf("eng cost %+v %v", eng.Cost, eng.UnpricedModels)
	}
	if ml := got[1]; ml.Cost != nil || len(ml.UnpricedModels) != 1 {
		t.Fatalf("ml %+v", ml)
	}
}
