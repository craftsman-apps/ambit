package local

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// #108 X1: the local test call goes straight to 127.0.0.1, also when HTTPS_PROXY and HTTP_PROXY point at a proxy
// that does not answer (a company laptop).
func TestHTTPClientIgnoresProxyVariables(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := "http://" + dead.Addr().String()
	dead.Close()
	for _, v := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "ALL_PROXY"} {
		t.Setenv(v, proxy)
	}
	t.Setenv("NO_PROXY", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	resp, err := HTTPClient(atoi(t, port)).Get("http://llm.localtest.me:" + port + "/")
	if err != nil {
		t.Fatalf("request went to the proxy: %v", err)
	}
	resp.Body.Close()
}

// #108 X1: when nothing answered, the final error names the last cause (here: the connection was refused), and is
// never empty or only "context deadline exceeded".
func TestCallWithFinalErrorNamesTheCause(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens: every request is refused
	_, _, err = CallWith(context.Background(), http.DefaultClient, "http://"+addr, "tok", "m", FormatChat, 100*time.Millisecond, 10*time.Millisecond)
	var na *NoAnswerError
	if !errors.As(err, &na) || !strings.Contains(err.Error(), "connection refused") && !strings.Contains(err.Error(), "actively refused") {
		t.Fatalf("err = %v", err)
	}
	// A server that never answers: every request is cut by the window's end, so there is no earlier cause.
	stop := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer hang.Close()
	defer close(stop) // runs first, so Close does not wait for a handler
	_, _, err = CallWith(context.Background(), http.DefaultClient, hang.URL, "tok", "m", FormatChat, 100*time.Millisecond, 10*time.Millisecond)
	if !errors.As(err, &na) || !strings.Contains(err.Error(), "no answer: each request was still waiting") || strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("err = %v", err)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
