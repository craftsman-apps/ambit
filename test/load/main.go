// Command load sends requests at a fixed rate (open loop: a slow answer does not slow the sender) and reports
// latency percentiles after a warm-up. For the model-routing performance rows (spec, Performance).
package main

import (
	"bytes"
	"encoding/json"
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

// bodyFor alternates the request bodies (request i uses body i mod n): with two, half the load is each cohort.
func bodyFor(i int, bodies [][]byte) []byte { return bodies[i%len(bodies)] }

// answerLine is one JSON line for -answers-file: the status, and the model and content of a non-streamed answer
// (the mock echoes the routing headers in its content).
func answerLine(status int, body []byte) string {
	var a struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(body, &a)
	content := ""
	if len(a.Choices) > 0 {
		content = a.Choices[0].Message.Content
	}
	b, _ := json.Marshal(map[string]any{"status": status, "model": a.Model, "content": content})
	return string(b)
}

func main() {
	url := flag.String("url", "http://llm.localtest.me:8080/v1/chat/completions", "endpoint")
	rate := flag.Float64("rate", 20, "requests per second")
	dur := flag.Duration("duration", 10*time.Minute, "total run time")
	warm := flag.Duration("warmup", 2*time.Minute, "excluded from results")
	tokenFile := flag.String("token-file", "", "file with a bearer token; read again for every request, so a script can refresh it during a long run")
	bodyFile := flag.String("body-file", "", "file with the JSON request body")
	bodyFile2 := flag.String("body-file2", "", "optional second body: requests alternate, and each cohort gets its own percentiles")
	answersFile := flag.String("answers-file", "", "optional: one JSON line per measured request (status, model, content)")
	flag.Parse()
	var bodies [][]byte
	for _, f := range []string{*bodyFile, *bodyFile2} {
		if f == "" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		bodies = append(bodies, b)
	}
	var answers *os.File
	if *answersFile != "" {
		f, err := os.Create(*answersFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer f.Close()
		answers = f
	}
	var mu sync.Mutex
	lat := make([][]float64, len(bodies))
	offered, completed, errs := make([]int, len(bodies)), make([]int, len(bodies)), make([]int, len(bodies))
	var wg sync.WaitGroup
	start := time.Now()
	tick := time.NewTicker(time.Duration(float64(time.Second) / *rate))
	defer tick.Stop()
	i := 0
	for now := range tick.C {
		if now.Sub(start) > *dur {
			break
		}
		cohort := i % len(bodies)
		body := bodyFor(i, bodies)
		i++
		measured := now.Sub(start) >= *warm
		if measured {
			offered[cohort]++
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
			var rest []byte
			if ok { // time to first token: the first byte of the streamed body
				one := make([]byte, 1)
				n, rerr := resp.Body.Read(one)
				ok = rerr == nil || (rerr == io.EOF && n == 1)
				ms = float64(time.Since(sent).Microseconds()) / 1000
				rest = one[:n]
			}
			status := 0
			if resp != nil {
				status = resp.StatusCode
				more, _ := io.ReadAll(resp.Body)
				rest = append(rest, more...)
				resp.Body.Close()
			}
			mu.Lock()
			defer mu.Unlock()
			if !measured {
				return
			}
			if answers != nil {
				fmt.Fprintln(answers, answerLine(status, rest))
			}
			if ok {
				completed[cohort]++
				lat[cohort] = append(lat[cohort], ms)
			} else {
				errs[cohort]++
			}
		}(now)
	}
	wg.Wait()
	for c := range bodies {
		p := percentiles(lat[c])
		prefix := ""
		if len(bodies) > 1 {
			prefix = fmt.Sprintf("cohort%d ", c+1)
		}
		fmt.Printf("%sp50=%.1fms p95=%.1fms p99=%.1fms offered=%d completed=%d errors=%d\n", prefix, p[50], p[95], p[99], offered[c], completed[c], errs[c])
	}
}
