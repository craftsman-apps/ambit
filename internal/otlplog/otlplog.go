// Package otlplog sends Ambit's own records (the guard's compression records, the controller's access snapshots) to
// the OpenTelemetry collector over OTLP/gRPC, the endpoint the gateway uses, so they reach the same otel_logs table
// with the same retention. Sending is best effort: a slow or absent collector costs records, never the caller's work.
package otlplog

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	Queue   = 1000            // records waiting to be sent; a full queue drops the new record
	Batch   = 100             // records in one export call
	Flush   = time.Second     // longest wait before a partly filled batch is sent
	Timeout = 5 * time.Second // one export call
)

// Drop reasons. The caller's counter has a "reason" label.
const (
	DropQueueFull  = "queue_full"
	DropSendFailed = "send_failed"
	DropClosed     = "closed" // Send after Close
)

// Exporter queues records and sends them in batches. Send never blocks and never fails: a record that cannot be
// kept is dropped and counted in Dropped.
type Exporter struct {
	conn    *grpc.ClientConn
	logs    collogs.LogsServiceClient
	service string
	dropped *prometheus.CounterVec
	queue   chan *logspb.LogRecord
	done    chan struct{}
	stop    chan struct{}      // closed by Close; the queue itself is never closed, so a late Send cannot panic
	flush   chan chan struct{} // Flush requests
	once    sync.Once
	mu      sync.RWMutex // Send holds it shared while it queues; Close holds it to refuse later records
	closed  bool
	warn    time.Time // last logged send failure
}

// New starts an exporter for the collector at endpoint (host:port, no TLS, as the gateway's own export). service is
// the record's service.name. It does not connect until the first batch, so a collector that starts later is no problem.
func New(endpoint, service string, dropped *prometheus.CounterVec) (*Exporter, error) {
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	e := newExporter(service, dropped, Queue)
	e.conn, e.logs = conn, collogs.NewLogsServiceClient(conn)
	go e.run()
	return e, nil
}

func newExporter(service string, dropped *prometheus.CounterVec, queue int) *Exporter {
	// Create every series at 0 now: a series that first appears at its first loss has no earlier sample, so
	// increase() would not see that loss and an alert on it would stay silent.
	for _, r := range []string{DropQueueFull, DropSendFailed, DropClosed} {
		dropped.WithLabelValues(r)
	}
	return &Exporter{service: service, dropped: dropped, queue: make(chan *logspb.LogRecord, queue), done: make(chan struct{}), stop: make(chan struct{}), flush: make(chan chan struct{})}
}

// Send queues rec. It returns at once.
func (e *Exporter) Send(rec *logspb.LogRecord) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed { // the worker has stopped or is about to; nothing would send this record
		e.dropped.WithLabelValues(DropClosed).Inc()
		return
	}
	select {
	case e.queue <- rec:
	default:
		e.dropped.WithLabelValues(DropQueueFull).Inc()
	}
}

// Flush sends what is queued now and returns when that is done (for tests and for a shutdown that must not lose records).
// It does nothing after Close.
func (e *Exporter) Flush() {
	ack := make(chan struct{})
	select {
	case e.flush <- ack:
		<-ack
	case <-e.done:
	}
}

// Close sends what is queued and stops. It waits at most Timeout: with the collector down every batch would take
// that long, and the process must still exit. Records not sent by then are lost, as when the collector is down.
func (e *Exporter) Close() {
	e.once.Do(func() {
		e.mu.Lock() // waits for Sends that are queuing now; the worker drains them after stop
		e.closed = true
		e.mu.Unlock()
		close(e.stop)
	})
	select {
	case <-e.done:
	case <-time.After(Timeout):
	}
	_ = e.conn.Close()
}

func (e *Exporter) run() {
	defer close(e.done)
	tick := time.NewTicker(Flush)
	defer tick.Stop()
	var batch []*logspb.LogRecord
	flush := func() {
		if len(batch) > 0 {
			e.export(batch)
			batch = batch[:0]
		}
	}
	drain := func() { // everything queued now
		for {
			select {
			case rec := <-e.queue:
				if batch = append(batch, rec); len(batch) >= Batch {
					flush()
				}
			default:
				return
			}
		}
	}
	for {
		select {
		case rec := <-e.queue:
			if batch = append(batch, rec); len(batch) >= Batch {
				flush()
			}
		case <-tick.C:
			flush()
		case ack := <-e.flush:
			drain()
			flush()
			close(ack)
		case <-e.stop:
			drain()
			flush()
			return
		}
	}
}

func (e *Exporter) export(batch []*logspb.LogRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	req := &collogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  &resourcepb.Resource{Attributes: []*commonpb.KeyValue{Str("service.name", e.service)}},
		ScopeLogs: []*logspb.ScopeLogs{{LogRecords: batch}},
	}}}
	if _, err := e.logs.Export(ctx, req); err != nil {
		e.dropped.WithLabelValues(DropSendFailed).Add(float64(len(batch)))
		if time.Since(e.warn) > time.Minute { // one line a minute, not one per batch
			e.warn = time.Now()
			slog.Warn("records not delivered to the collector", "service", e.service, "dropped", len(batch), "err", err)
		}
	}
}

// Str is a string attribute.
func Str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// Record is an INFO log record at t with a string body.
func Record(t time.Time, body string, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	return &logspb.LogRecord{
		TimeUnixNano: uint64(t.UnixNano()), ObservedTimeUnixNano: uint64(t.UnixNano()),
		SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO, SeverityText: "INFO",
		Body:       &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		Attributes: attrs,
	}
}
