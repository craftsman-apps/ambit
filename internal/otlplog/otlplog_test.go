package otlplog

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

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
}
