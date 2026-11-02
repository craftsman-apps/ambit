package guard

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// RecordKind marks a guard record in otel_logs. The savings summary view filters on it.
const RecordKind = "compression"

const (
	recordQueue   = 1000            // records waiting to be sent; a full queue drops the new record
	recordBatch   = 100             // records in one export call
	recordFlush   = time.Second     // longest wait before a partly filled batch is sent
	recordTimeout = 5 * time.Second // one export call
)

// Drop reasons of ambit_guard_record_dropped_total.
const (
	dropNoGroups   = "no_groups"
	dropQueueFull  = "queue_full"
	dropSendFailed = "send_failed"
)

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

// Recorder sends records to an OpenTelemetry collector over OTLP/gRPC, the endpoint the gateway uses, so
// they reach the same otel_logs table. The queue is bounded; a slow or absent collector costs records, never calls.
type Recorder struct {
	conn  *grpc.ClientConn
	logs  collogs.LogsServiceClient
	queue chan CompressionRecord
	done  chan struct{}
	once  sync.Once
	warn  time.Time // last logged send failure
}

// NewRecorder starts a recorder for the collector at endpoint (host:port, no TLS, as the gateway's own export).
// It does not connect until the first record, so a collector that starts later is no problem.
func NewRecorder(endpoint string) (*Recorder, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	r := &Recorder{conn: conn, logs: collogs.NewLogsServiceClient(conn), queue: make(chan CompressionRecord, recordQueue), done: make(chan struct{})}
	go r.run()
	return r, nil
}

// Record queues r for sending. It returns at once.
func (r *Recorder) Record(rec CompressionRecord) {
	select {
	case r.queue <- rec:
	default:
		recordDropped.WithLabelValues(dropQueueFull).Inc()
	}
}

// Close sends what is queued (up to recordTimeout per batch) and stops.
func (r *Recorder) Close() {
	r.once.Do(func() { close(r.queue) })
	<-r.done
	_ = r.conn.Close()
}

func (r *Recorder) run() {
	defer close(r.done)
	tick := time.NewTicker(recordFlush)
	defer tick.Stop()
	var batch []CompressionRecord
	flush := func() {
		if len(batch) > 0 {
			r.export(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case rec, ok := <-r.queue:
			if !ok {
				flush()
				return
			}
			if batch = append(batch, rec); len(batch) >= recordBatch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (r *Recorder) export(batch []CompressionRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if _, err := r.logs.Export(ctx, logRequest(batch, time.Now())); err != nil {
		recordDropped.WithLabelValues(dropSendFailed).Add(float64(len(batch)))
		if time.Since(r.warn) > time.Minute { // one line a minute, not one per batch
			r.warn = time.Now()
			slog.Warn("guard: compression records not delivered to the collector", "dropped", len(batch), "err", err)
		}
	}
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// logRequest builds the OTLP export for a batch. jwt.groups is a JSON array in a string, the format of the
// gateway's own records, so one extraction works for both.
func logRequest(batch []CompressionRecord, now time.Time) *collogs.ExportLogsServiceRequest {
	recs := make([]*logspb.LogRecord, 0, len(batch))
	for _, b := range batch {
		groups := b.Groups
		if groups == nil {
			groups = []string{}
		}
		gj, _ := json.Marshal(groups)
		recs = append(recs, &logspb.LogRecord{
			TimeUnixNano: uint64(now.UnixNano()), ObservedTimeUnixNano: uint64(now.UnixNano()),
			SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO, SeverityText: "INFO",
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: RecordKind}},
			Attributes: []*commonpb.KeyValue{
				str("ambit.kind", RecordKind), str("mcp.target", b.Server),
				str("ambit.compress.type", b.Type), str("ambit.compress.mode", b.Mode),
				str("ambit.compress.bytes_in", strconv.Itoa(b.BytesIn)), str("ambit.compress.bytes_out", strconv.Itoa(b.BytesOut)),
				str("ambit.compress.omitted", strconv.Itoa(b.Omitted)), str("jwt.groups", string(gj)),
			},
		})
	}
	return &collogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "ambit-guard")}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: recs}},
	}}}
}
