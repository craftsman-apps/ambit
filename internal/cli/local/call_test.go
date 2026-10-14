package local

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gateway starts a test server on 127.0.0.1 and returns its port; handler answers every request.
func gateway(t *testing.T, h http.HandlerFunc) int {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.Listener.Addr().(*net.TCPAddr).Port
}

func TestDexTokenThroughLocaltestMe(t *testing.T) {
	port := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.URL.Path != "/dex/token" || r.Host != "console.localtest.me:"+strings.Split(r.Host, ":")[1] ||
			r.Form.Get("grant_type") != "password" || r.Form.Get("client_id") != "ambit-cli" ||
			r.Form.Get("username") != "admin@ambit.local" || r.Form.Get("password") != "pw-1234" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Write([]byte(`{"access_token":"tok-123","token_type":"bearer"}`))
	})
	c := HTTPClient(port)
	tok, err := DexToken(context.Background(), c, ConsoleURL(port), "pw-1234")
	if err != nil || tok != "tok-123" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if _, err := DexToken(context.Background(), c, ConsoleURL(port), "wrong"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("wrong password: %v", err)
	}
}

func sequence(codes ...int) http.HandlerFunc {
	var i atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		code := codes[min(int(i.Add(1))-1, len(codes)-1)]
		w.WriteHeader(code)
		if code == 200 {
			w.Write([]byte(`{"choices":[{"message":{"content":"Hello there, nice to meet!\nSecond line"}}]}`))
		} else {
			w.Write([]byte(`{"error":{"message":"status ` + http.StatusText(code) + `"}}`))
		}
	}
}

func TestTestCallSettlesThenAnswers(t *testing.T) {
	port := gateway(t, sequence(404, 403, 200))
	got, err := TestCall(context.Background(), HTTPClient(port), LLMURL(port), "tok", "local/echo", 5*time.Second, 10*time.Millisecond)
	if err != nil || got != "Hello there, nice to meet!" {
		t.Fatalf("= %q, %v", got, err)
	}
}

func TestTestCallProviderErrorStopsAtOnce(t *testing.T) {
	port := gateway(t, sequence(404, 429))
	_, err := TestCall(context.Background(), HTTPClient(port), LLMURL(port), "tok", "m:free", 5*time.Second, 10*time.Millisecond)
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != 429 {
		t.Fatalf("err = %v", err)
	}
}

func TestTestCallTimeoutNamesLastAnswer(t *testing.T) {
	port := gateway(t, sequence(403))
	_, err := TestCall(context.Background(), HTTPClient(port), LLMURL(port), "tok", "local/echo", 50*time.Millisecond, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no answer within 50ms; the last answer was HTTP 403") {
		t.Fatalf("err = %v", err)
	}
}

func TestTestCallOurOwnRefusalIsNotAProviderError(t *testing.T) {
	for code, want := range map[int]string{401: "the token or access rule is wrong", 400: "the request was rejected"} {
		port := gateway(t, sequence(code))
		_, err := TestCall(context.Background(), HTTPClient(port), LLMURL(port), "tok", "local/echo", 5*time.Second, 10*time.Millisecond)
		var pe *ProviderError
		if err == nil || errors.As(err, &pe) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), strconv.Itoa(code)) {
			t.Fatalf("HTTP %d: err = %v", code, err)
		}
	}
}

func TestTestCallUpstreamFailureIsAProviderError(t *testing.T) {
	for _, code := range []int{429, 500, 504} {
		port := gateway(t, sequence(code))
		_, err := TestCall(context.Background(), HTTPClient(port), LLMURL(port), "tok", "m:free", 5*time.Second, 10*time.Millisecond)
		var pe *ProviderError
		if !errors.As(err, &pe) || pe.Status != code {
			t.Fatalf("HTTP %d: err = %v", code, err)
		}
	}
}

func TestTestCallHangingGatewayStopsAtTheLimit(t *testing.T) {
	port := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	start := time.Now()
	_, err := TestCall(context.Background(), HTTPClient(port), LLMURL(port), "tok", "local/echo", 100*time.Millisecond, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no answer within 100ms") || time.Since(start) > 2*time.Second {
		t.Fatalf("after %s: err = %v", time.Since(start), err)
	}
}
