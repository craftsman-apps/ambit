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
