// Command load sends requests at a fixed rate (open loop: a slow answer does not slow the sender) and reports
// latency percentiles after a warm-up. For the model-routing performance rows (spec, Performance).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func percentiles(d []float64) map[int]float64 {
	out := map[int]float64{}
	if len(d) == 0 {
		return out
	}
	s := append([]float64(nil), d...)
	sort.Float64s(s)
	for _, p := range []int{50, 95, 99} {
		i := (p*len(s)+99)/100 - 1
		out[p] = s[i]
	}
	return out
}

func main() {
	url := flag.String("url", "http://llm.localtest.me:8080/v1/chat/completions", "endpoint")
	rate := flag.Float64("rate", 20, "requests per second")
	dur := flag.Duration("duration", 10*time.Minute, "total run time")
	warm := flag.Duration("warmup", 2*time.Minute, "excluded from results")
	tokenFile := flag.String("token-file", "", "file with a bearer token; read again for every request, so a script can refresh it during a long run")
	bodyFile := flag.String("body-file", "", "file with the JSON request body")
	flag.Parse()
	body, err := os.ReadFile(*bodyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var mu sync.Mutex
	var lat []float64
	var offered, completed, errs int
	var wg sync.WaitGroup
	start := time.Now()
	tick := time.NewTicker(time.Duration(float64(time.Second) / *rate))
	defer tick.Stop()
	for now := range tick.C {
		if now.Sub(start) > *dur {
			break
		}
		measured := now.Sub(start) >= *warm
		if measured {
			offered++
		}
		wg.Add(1)
		go func(sent time.Time) {
			defer wg.Done()
			req, _ := http.NewRequest("POST", *url, bytes.NewReader(body))
			tok, _ := os.ReadFile(*tokenFile)
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			ok := err == nil && resp.StatusCode == 200
			var ms float64
			if ok { // time to first token: the first byte of the streamed body
				one := make([]byte, 1)
				_, rerr := resp.Body.Read(one)
				ok = rerr == nil
				ms = float64(time.Since(sent).Microseconds()) / 1000
			}
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			mu.Lock()
			defer mu.Unlock()
			if !measured {
				return
			}
			if ok {
				completed++
				lat = append(lat, ms)
			} else {
				errs++
			}
		}(now)
	}
	wg.Wait()
	p := percentiles(lat)
	fmt.Printf("p50=%.1fms p95=%.1fms p99=%.1fms offered=%d completed=%d errors=%d\n", p[50], p[95], p[99], offered, completed, errs)
}
