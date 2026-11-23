package guard

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/craftsman-apps/ambit/internal/otlplog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// RecordKind marks a guard record in otel_logs. The savings summary view filters on it.
const RecordKind = "compression"

// Drop reason of ambit_guard_record_dropped_total beside otlplog's queue_full and send_failed.
const dropNoGroups = "no_groups"

var recordDropped = promauto.NewCounterVec(prometheus.CounterOpts{Name: "ambit_guard_record_dropped_total",
	Help: "Compression records the guard did not deliver to the collector, by reason."}, []string{"reason"})

// CompressionRecord is what the guard writes for one compressed text: sizes and names only, never content.
// The Savings summaries read only the groups; the email, key ID and call ID let the Audit page show the record on
// the row of its call (#203).
type CompressionRecord struct {
	Server   string
	Tool     string // the tool's own name, as in the gateway's record
	Type     string
	Mode     string
	Part     int // index of the text in content[]
	BytesIn  int
	BytesOut int
	Omitted  int
	Groups   []string // empty: the caller has no group
	Email    string   // the caller, as the access log writes jwt.email; empty with compressionAudit.caller false
	KeyID    string   // the personal or service account key of the call; empty for a token
	CallID   string   // the call, as the access log writes ambit.call_id; empty from an older gateway policy
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
	attrs := []*commonpb.KeyValue{str("ambit.kind", RecordKind), str("mcp.target", b.Server),
		str("ambit.compress.type", b.Type), str("ambit.compress.mode", b.Mode),
		str("ambit.compress.bytes_in", strconv.Itoa(b.BytesIn)), str("ambit.compress.bytes_out", strconv.Itoa(b.BytesOut)),
		str("ambit.compress.omitted", strconv.Itoa(b.Omitted)), str("ambit.compress.part", strconv.Itoa(b.Part)), str("jwt.groups", string(gj))}
	// Written only when known, so a query for a person or a call never matches an empty value (#203).
	for _, kv := range [][2]string{{"gen_ai.tool.name", b.Tool}, {"jwt.email", b.Email}, {"ambit.key_id", b.KeyID}, {"ambit.call_id", b.CallID}} {
		if kv[1] != "" {
			attrs = append(attrs, str(kv[0], kv[1]))
		}
	}
	return otlplog.Record(now, RecordKind, attrs...)
}

var str = otlplog.Str
