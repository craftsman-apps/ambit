// stopmeasure calls an MCP endpoint with a key every few milliseconds and prints one line per call: the time in
// milliseconds since the epoch and the HTTP status. test/stepagents.sh stops an agent while it runs and reads the lines to
// get the time from the stop request to the first refusal and the number of calls accepted after it.
//
//	stopmeasure -url http://mcp.localtest.me:8080/mcp -key ambit_... -every 25ms -connect 127.0.0.1:28080
//
// -connect sends every connection to that address whatever the URL says (the test cluster's own host port); it is
// required, so that a request never reaches another cluster on the usual port.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

func main() {
	url := flag.String("url", "", "MCP URL")
	key := flag.String("key", "", "the key")
	every := flag.Duration("every", 25*time.Millisecond, "time between calls")
	connect := flag.String("connect", "", "host:port every connection goes to (required)")
	flag.Parse()
	if *url == "" || *key == "" || *connect == "" {
		fmt.Fprintln(os.Stderr, "usage: stopmeasure -url URL -key KEY -connect HOST:PORT")
		os.Exit(2)
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, *connect)
		},
	}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"stopmeasure","version":"1"}}}`
	t := time.NewTicker(*every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", *url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+*key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		at := time.Now()
		resp, err := c.Do(req)
		status := 0
		if err == nil {
			status = resp.StatusCode
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		fmt.Printf("%d %d\n", at.UnixMilli(), status)
	}
}
