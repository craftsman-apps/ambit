package guard

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics of result compression, on /metrics of the guard's health port. They hold sizes and counts only,
// never content. The server label is the name of a Server object, type and outcome are fixed sets.
var (
	compressBytesIn  = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_guard_compress_bytes_in_total", Help: "Bytes of tool result text before compression."}, []string{"server", "type"})
	compressBytesOut = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_guard_compress_bytes_out_total", Help: "Bytes of tool result text after compression."}, []string{"server", "type"})
	compressResults  = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_guard_compress_results_total", Help: "Tool result texts seen by compression, by outcome."}, []string{"server", "type", "outcome"})
	compressOmitted  = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_guard_compress_omitted_total", Help: "Cuts made by shorten (places where content was left out)."}, []string{"server", "type"})
	compressSeconds  = promauto.NewHistogramVec(prometheus.HistogramOpts{Name: "ambit_guard_compress_seconds", Help: "Time of the compress step.", Buckets: prometheus.ExponentialBuckets(0.001, 4, 8)}, []string{"type"})
)
