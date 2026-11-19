package console

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// The load test of spec #89 (G3, not in CI): 5,000,000 gateway records over a year, exported in one ZIP. The console's
// Go memory must grow by under 64 MiB while the export streams. Run with AMBIT_EVIDENCE_LOAD=1 and a ClickHouse
// (AMBIT_TEST_CLICKHOUSE_*, as TestEvidenceOnClickHouse); it seeds a scratch database evidence_load.
func TestEvidenceExportMemory(t *testing.T) {
	if os.Getenv("AMBIT_EVIDENCE_LOAD") != "1" {
		t.Skip("AMBIT_EVIDENCE_LOAD is not 1")
	}
	a := clickhouseTestDB(t, "evidence_load")
	const n = 5_000_000
	start := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -364)
	seed := `INSERT INTO otel_logs (Timestamp, LogAttributes)
SELECT toDateTime64({start:DateTime64(9)}, 9) + toIntervalMillisecond(number * 6290),
       map('jwt.email', concat('user', toString(number % 500), '@example.com'), 'jwt.sub', concat('sub-', toString(number % 500)),
           'route', 'ambit-system/ambit-llm', 'gen_ai.request.model', 'gpt-5', 'http.status', if(number % 50 = 0, '403', '200'),
           'gen_ai.usage.input_tokens', toString(number % 4000), 'gen_ai.usage.output_tokens', toString(number % 900),
           'ambit.identity', 'key', 'ambit.key_id', concat('k', toString(number % 800)), 'ambit.accessMode', 'enforce',
           'ambit.rulesVersion', '3f9a1c0e2b7d', 'url.path', '/v1/chat/completions', 'protocol', 'llm')
FROM numbers({offset:UInt64}, 500000)
SETTINGS max_partitions_per_insert_block = 1000`
	t0 := time.Now()
	for off := 0; off < n; off += 500_000 { // in parts: one INSERT of 5,000,000 rows needs more memory than the chart gives ClickHouse
		if _, err := a.DB.ExecContext(seedContext(start, off), seed); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("seeded %d records in %v", n, time.Since(t0).Round(time.Second))

	e := newEvidenceFixture(t, nil)
	e.cat.Audit = a
	e.cat.Evidence.MaxRows = n
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // sample the heap in use while the export runs
		defer wg.Done()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				runtime.ReadMemStats(&m)
				peak = max(peak, m.HeapInuse)
			}
		}
	}()
	from, to := periodName(start, start.AddDate(0, 0, 365))
	req, _ := http.NewRequestWithContext(t.Context(), "GET", e.srv.URL+"/api/evidence/audit.zip?from="+from+"&to="+to, nil)
	req.AddCookie(e.admin)
	t1 := time.Now()
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	size, err := io.Copy(h, resp.Body)
	resp.Body.Close()
	close(stop)
	wg.Wait()
	took := time.Since(t1)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp.StatusCode, err)
	}
	hist := e.history(t)
	if len(hist) != 2 || hist[1].Result != "complete" || hist[1].Rows != n {
		t.Fatalf("history %+v", hist)
	}
	growth := int64(peak) - int64(base.HeapInuse)
	t.Logf("exported %d rows in %v, ZIP %d MiB; Go heap in use: %d MiB before, peak %d MiB, growth %d MiB",
		n, took.Round(time.Second), size>>20, base.HeapInuse>>20, peak>>20, growth>>20)
	if growth >= 64<<20 {
		t.Errorf("heap grew by %d MiB, the target is under 64 MiB", growth>>20)
	}
}

// seedContext has the seed's parameter; not exportContext, whose readonly setting refuses the INSERT.
func seedContext(start time.Time, offset int) context.Context {
	return clickhouse.Context(context.Background(), clickhouse.WithParameters(clickhouse.Parameters{"start": chTime(start), "offset": strconv.Itoa(offset)}))
}
