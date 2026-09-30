package console

import (
	"net/http/httptest"
	"net/url"
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
