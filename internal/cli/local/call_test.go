package local

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// recorder answers 200 with the body of the format and keeps the last request.
func recorder(t *testing.T, answer string) (port int, got *http.Request, body *[]byte) {
	var last http.Request
	var lastBody []byte
	port = gateway(t, func(w http.ResponseWriter, r *http.Request) {
		last = *r.Clone(r.Context())
		lastBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(answer))
	})
	return port, &last, &lastBody
}

func TestCallWithEachFormat(t *testing.T) {
	for _, tt := range []struct {
		name    string
		format  Format
		answer  string
		path    string
		auth    func(*http.Request) bool
		fields  map[string]any
		wantOut string
	}{
		{"chat", FormatChat, `{"choices":[{"message":{"content":"Hello chat\nmore"}}]}`, "/v1/chat/completions",
			func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer ambit_k" },
			map[string]any{"model": "m"}, "Hello chat"},
		{"messages", FormatMessages, `{"content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"Hello messages"}]}`, "/v1/messages",
			func(r *http.Request) bool {
				return r.Header.Get("x-api-key") == "ambit_k" && r.Header.Get("anthropic-version") == "2023-06-01" && r.Header.Get("Authorization") == ""
			},
			map[string]any{"model": "m", "max_tokens": float64(16)}, "Hello messages"},
		{"responses", FormatResponses, `{"output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"Hello responses"}]}]}`, "/v1/responses",
			func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer ambit_k" },
			map[string]any{"model": "m", "max_output_tokens": float64(16), "input": Prompt}, "Hello responses"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			port, req, body := recorder(t, tt.answer)
			got, err := CallWith(context.Background(), HTTPClient(port), LLMURL(port), "ambit_k", "m", tt.format, 5*time.Second, 10*time.Millisecond)
			if err != nil || got != tt.wantOut {
				t.Fatalf("= %q, %v", got, err)
			}
			if req.URL.Path != tt.path || req.Method != "POST" || !tt.auth(req) || req.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("request %s %s %v", req.Method, req.URL.Path, req.Header)
			}
			var m map[string]any
			if err := json.Unmarshal(*body, &m); err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.fields {
				if m[k] != v {
					t.Errorf("body[%s] = %v, want %v (%s)", k, m[k], v, *body)
				}
			}
			if !strings.Contains(string(*body), Prompt) {
				t.Errorf("the prompt is not in the body: %s", *body)
			}
		})
	}
}

func TestCallWithAnswerWithoutTextIsAnError(t *testing.T) {
	for _, f := range []Format{FormatChat, FormatMessages, FormatResponses} {
		port, _, _ := recorder(t, `{"unexpected":true}`)
		if _, err := CallWith(context.Background(), HTTPClient(port), LLMURL(port), "k", "m", f, time.Second, 10*time.Millisecond); err == nil || !strings.Contains(err.Error(), "HTTP 200 without an answer") {
			t.Errorf("format %d: err = %v", f, err)
		}
	}
}

// What the connect command maps to its lines: 401 is a RefusedError, 403 and 404 end as NoAnswerError with the
// last status, and a gateway that is not there is a NoAnswerError with status 0.
func TestCallWithErrorsCarryTheStatus(t *testing.T) {
	port := gateway(t, sequence(401))
	_, err := CallWith(context.Background(), HTTPClient(port), LLMURL(port), "k", "m", FormatMessages, time.Second, 10*time.Millisecond)
	var re *RefusedError
	if !errors.As(err, &re) || re.Status != 401 {
		t.Fatalf("401: %v", err)
	}
	for _, code := range []int{403, 404} {
		port := gateway(t, sequence(code))
		_, err := CallWith(context.Background(), HTTPClient(port), LLMURL(port), "k", "m", FormatResponses, 50*time.Millisecond, 10*time.Millisecond)
		var na *NoAnswerError
		if !errors.As(err, &na) || na.LastStatus != code {
			t.Fatalf("%d: %v", code, err)
		}
	}
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close()
	_, err = CallWith(context.Background(), http.DefaultClient, url, "k", "m", FormatChat, 50*time.Millisecond, 10*time.Millisecond)
	var na *NoAnswerError
	if !errors.As(err, &na) || na.LastStatus != 0 {
		t.Fatalf("connection refused: %v", err)
	}
}
