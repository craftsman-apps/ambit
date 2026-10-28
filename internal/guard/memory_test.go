package guard

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/extmcp"
)

// bigResult is a tools/call result with one text of size bytes and an email at its end.
func bigResult(size int) []byte {
	text := strings.Repeat("customer record line\n", size/21) + "contact: someone@example.com"
	b, _ := json.Marshal(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
	return b
}

// peakHeap runs f and returns the most heap in use at any 1 ms sample, minus what was in use before
// (the input the caller holds is part of that baseline).
func peakHeap(f func()) uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	base := m.HeapInuse
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var s runtime.MemStats
		for {
			runtime.ReadMemStats(&s)
			if s.HeapInuse > peak.Load() {
				peak.Store(s.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	f()
	close(stop)
	<-done
	return max(peak.Load(), base) - base
}

func checkBig(t testing.TB, g *Guard, in []byte) {
	res, err := g.CheckResponse(t.Context(), &extmcp.McpResponse{ServiceNames: []string{"crm"}, Method: "tools/call", McpResponse: in})
	if err != nil || !strings.Contains(string(res.GetMutated()), "<EMAIL>") {
		t.Fatalf("not masked: %v", err)
	}
}

// BenchmarkMaskResult reports the peak extra heap (as a multiple of the result size) for 10 and 30 MiB results.
func BenchmarkMaskResult(b *testing.B) {
	for _, mib := range []int{10, 30} {
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			in := bigResult(mib << 20)
			g := &Guard{Masker: Builtin{}}
			for range b.N {
				p := peakHeap(func() { checkBig(b, g, append([]byte(nil), in...)) })
				b.ReportMetric(float64(p)/float64(len(in)), "peak/size")
				b.ReportMetric(float64(p)/(1<<20), "peakMiB")
			}
		})
	}
}

// A 10 MiB result must not need more than about 5 times its size on top of the input (issue 190: before the
// fix it needed 5.8 times, and a 128 MiB container ran out of memory at 12 MiB).
func TestMaskResultPeakMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("samples the heap")
	}
	in := bigResult(10 << 20)
	g := &Guard{Masker: Builtin{}}
	p := peakHeap(func() { checkBig(t, g, append([]byte(nil), in...)) })
	t.Logf("peak extra heap %.1f MiB for a %.1f MiB result (%.1f times)", float64(p)/(1<<20), float64(len(in))/(1<<20), float64(p)/float64(len(in)))
	if p > 5*uint64(len(in)) {
		t.Fatalf("peak extra heap is %d bytes, more than 5 times the result (%d bytes)", p, len(in))
	}
}
