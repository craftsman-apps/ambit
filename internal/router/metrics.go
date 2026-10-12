package router

import "github.com/prometheus/client_golang/prometheus"

// Metrics: reasons per router, and the classifier's tokens (not charged to budgets in v1, spec "Cost").
type Metrics struct {
	requests *prometheus.CounterVec
	tokens   *prometheus.CounterVec
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ambit_router_requests_total", Help: "Routed requests by router and reason."}, []string{"router", "reason"}),
		tokens:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ambit_router_classifier_tokens_total", Help: "Tokens used by classifier calls."}, []string{"router", "model"}),
	}
	reg.MustRegister(m.requests, m.tokens)
	return m
}

func (m *Metrics) Reason(router, reason string) {
	if m != nil && router != "" {
		m.requests.WithLabelValues(router, reason).Inc()
	}
}

func (m *Metrics) Tokens(router, model string, n int) {
	if m != nil && n > 0 {
		m.tokens.WithLabelValues(router, model).Add(float64(n))
	}
}
