package otlplog

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// flakyCollector refuses its first `fail` Export calls with Unavailable (a collector that is restarting, or a
// connection that is not up yet) and then keeps what it gets.
type flakyCollector struct {
	collogs.UnimplementedLogsServiceServer
	mu    sync.Mutex
	fail  int
	calls int
	got   int
}

func (f *flakyCollector) Export(_ context.Context, r *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.fail {
		return nil, status.Error(codes.Unavailable, "collector not ready")
	}
	for _, rl := range r.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			f.got += len(sl.LogRecords)
		}
	}
	return &collogs.ExportLogsServiceResponse{}, nil
}

func (f *flakyCollector) state() (calls, got int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.got
}

func startFlaky(t *testing.T, fail int) (*flakyCollector, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &flakyCollector{fail: fail}
	s := grpc.NewServer()
	collogs.RegisterLogsServiceServer(s, f)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return f, lis.Addr().String()
}

// #415: a batch whose export fails is sent again, so one failed export (the first connection after a pod start, a
// collector restart) does not lose the record. Here the first two exports fail and the third succeeds.
func TestFailedExportIsRetried(t *testing.T) {
	old := RetryWait
	RetryWait = 10 * time.Millisecond
	t.Cleanup(func() { RetryWait = old })
	f, addr := startFlaky(t, 2)
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_retry_dropped_total"}, []string{"reason"})
	e, err := New(addr, "test", dropped)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Send(Record(time.Now(), "injection"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, got := f.state(); got == 1 {
			break
		}
		if time.Now().After(deadline) {
			calls, got := f.state()
			t.Fatalf("record not delivered after %d export calls (got %d); send_failed=%v", calls, got, testutil.ToFloat64(dropped.WithLabelValues(DropSendFailed)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d := testutil.ToFloat64(dropped.WithLabelValues(DropSendFailed)); d != 0 {
		t.Errorf("send failures counted %v, want 0: the record was delivered", d)
	}
}

// When every try fails the batch is dropped once and counted once per record, not once per try, and after Retries
// retries (so a collector that stays down costs no more than that).
func TestExportGivesUpAfterRetries(t *testing.T) {
	old := RetryWait
	RetryWait = 10 * time.Millisecond
	t.Cleanup(func() { RetryWait = old })
	f, addr := startFlaky(t, 1000)
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_giveup_dropped_total"}, []string{"reason"})
	e, err := New(addr, "test", dropped)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.Send(Record(time.Now(), "a"))
	e.Send(Record(time.Now(), "b"))
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(dropped.WithLabelValues(DropSendFailed)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the batch was not dropped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d := testutil.ToFloat64(dropped.WithLabelValues(DropSendFailed)); d != 2 {
		t.Errorf("send failures counted %v, want 2 (two records, one batch)", d)
	}
	if calls, _ := f.state(); calls != 1+Retries {
		t.Errorf("%d export calls, want %d (one try and %d retries)", calls, 1+Retries, Retries)
	}
}

// Flush and Close are not retried: a shutdown must finish within Timeout, and tests with a dead collector count at once.
func TestFlushIsNotRetried(t *testing.T) {
	f, addr := startFlaky(t, 1000)
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_flushnoretry_dropped_total"}, []string{"reason"})
	e, err := New(addr, "test", dropped)
	if err != nil {
		t.Fatal(err)
	}
	e.Send(Record(time.Now(), "x"))
	start := time.Now()
	e.Flush()
	if calls, _ := f.state(); calls != 1 {
		t.Errorf("Flush made %d export calls, want 1", calls)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Errorf("Flush took %v; it must not wait to retry", time.Since(start))
	}
	e.Close()
}

// A full queue drops and counts; Send never blocks.
func TestFullQueueDrops(t *testing.T) {
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_dropped_total"}, []string{"reason"})
	e := newExporter("test", dropped, 2) // nobody reads the queue
	done := make(chan struct{})
	go func() {
		for range 5 {
			e.Send(Record(time.Now(), "x"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked")
	}
	if d := testutil.ToFloat64(dropped.WithLabelValues(DropQueueFull)); d != 3 {
		t.Errorf("dropped %v, want 3", d)
	}
}

// Flush sends what is queued before it returns, also when the collector is down (then the records are counted).
func TestFlushSendsQueued(t *testing.T) {
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_flush_dropped_total"}, []string{"reason"})
	e, err := New("127.0.0.1:1", "test", dropped) // nothing listens here
	if err != nil {
		t.Fatal(err)
	}
	e.Send(Record(time.Now(), "x"))
	e.Send(Record(time.Now(), "y"))
	e.Flush()
	if d := testutil.ToFloat64(dropped.WithLabelValues(DropSendFailed)); d != 2 {
		t.Errorf("send failures %v, want 2", d)
	}
}

// A record sent while or after Close must not panic (a request may still be recording when the process stops).
func TestSendAfterCloseDoesNotPanic(t *testing.T) {
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_close_dropped_total"}, []string{"reason"})
	e, err := New("127.0.0.1:1", "test", dropped)
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	e.Send(Record(time.Now(), "late"))
	e.Flush() // returns: the worker has stopped
	if d := testutil.ToFloat64(dropped.WithLabelValues(DropClosed)); d != 1 {
		t.Errorf("late record counted %v times as closed, want 1", d)
	}
}

// Every reason is exposed at 0 from the start, so increase() sees the first loss.
func TestReasonsExposedAtZero(t *testing.T) {
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_zero_dropped_total"}, []string{"reason"})
	newExporter("test", dropped, 1)
	if n := testutil.CollectAndCount(dropped); n != 3 {
		t.Errorf("%d series exposed before any loss, want 3 (queue_full, send_failed, closed)", n)
	}
}

// A record queued before Close is sent (and counted here, the collector being down); none is lost silently.
func TestCloseSendsRecordsQueuedBefore(t *testing.T) {
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_close_sends_total"}, []string{"reason"})
	e, err := New("127.0.0.1:1", "test", dropped)
	if err != nil {
		t.Fatal(err)
	}
	e.Send(Record(time.Now(), "before"))
	e.Close()
	if d := testutil.ToFloat64(dropped.WithLabelValues(DropSendFailed)); d != 1 {
		t.Errorf("record queued before Close: send failures %v, want 1", d)
	}
}
