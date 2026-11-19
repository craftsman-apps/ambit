package guard

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/craftsman-apps/ambit/internal/otlplog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// RecordKind marks a guard record in otel_logs. The savings summary view filters on it.
const RecordKind = "compression"

// Drop reason of ambit_guard_record_dropped_total beside otlplog's queue_full and send_failed.
const dropNoGroups = "no_groups"

var recordDropped = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_guard_record_dropped_total",
	Help: "Compression records the guard did not deliver to the collector, by reason."}, []string{"reason"})

// CompressionRecord is what the guard writes for one compressed text: sizes and names only, never content.
// It has the caller's groups and deliberately no email (Feature B counts groups; see docs/savings.md).
type CompressionRecord struct {
	Server   string
	Type     string
	Mode     string
	BytesIn  int
	BytesOut int
	Omitted  int
	Groups   []string // empty: the caller has no group
}

// RecordSink receives compression records. Record never blocks and never fails: a record that cannot be kept is dropped and counted.
type RecordSink interface {
	Record(CompressionRecord)
}

// Recorder sends records to the OpenTelemetry collector (otlplog), so they reach the same otel_logs table as the
// gateway's. The queue is bounded; a slow or absent collector costs records, never calls.
type Recorder struct{ exp *otlplog.Exporter }

// NewRecorder starts a recorder for the collector at endpoint (host:port, no TLS, as the gateway's own export).
// It does not connect until the first record, so a collector that starts later is no problem.
func NewRecorder(endpoint string) (*Recorder, error) {
	exp, err := otlplog.New(endpoint, "ambit-guard", recordDropped)
	if err != nil {
		return nil, err
	}
	return &Recorder{exp: exp}, nil
}

// Record queues r for sending. It returns at once.
func (r *Recorder) Record(rec CompressionRecord) { r.exp.Send(logRecord(rec, time.Now())) }

// Close sends what is queued and stops, within otlplog.Timeout.
func (r *Recorder) Close() { r.exp.Close() }

// logRecord is the OTLP record of b. jwt.groups is a JSON array in a string, the format of the gateway's own
// records, so one extraction works for both.
func logRecord(b CompressionRecord, now time.Time) *logspb.LogRecord {
	groups := b.Groups
	if groups == nil {
		groups = []string{}
	}
	gj, _ := json.Marshal(groups)
	return otlplog.Record(now, RecordKind,
		str("ambit.kind", RecordKind), str("mcp.target", b.Server),
		str("ambit.compress.type", b.Type), str("ambit.compress.mode", b.Mode),
		str("ambit.compress.bytes_in", strconv.Itoa(b.BytesIn)), str("ambit.compress.bytes_out", strconv.Itoa(b.BytesOut)),
		str("ambit.compress.omitted", strconv.Itoa(b.Omitted)), str("jwt.groups", string(gj)))
}

var str = otlplog.Str
