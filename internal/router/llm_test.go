package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/routerconfig"
)

func TestLLMRequestAndAnswer(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"coding"}}],"usage":{"total_tokens":42}}`))
	}))
	defer srv.Close()
	r := auto
	r.LLM = &routerconfig.LLM{Endpoint: srv.URL + "/v1", Model: "qwen"}
	ans, n, err := (&LLM{HTTP: srv.Client()}).Classify(t.Context(), r, "sk-test", "fix my python")
	if err != nil || ans != "coding" || n != 42 || auth != "Bearer sk-test" {
		t.Fatalf("%q %d %v auth=%q", ans, n, err, auth)
	}
	msgs := got["messages"].([]any)
	if got["model"] != "qwen" || got["temperature"] != 0.0 || got["max_tokens"] != 8.0 ||
		!strings.Contains(msgs[0].(map[string]any)["content"].(string), "coding: code") || msgs[1].(map[string]any)["content"] != "fix my python" {
		t.Fatalf("request %v", got)
	}
}

func TestAuthorizationHeader(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"other"}}]}`))
	}))
	defer srv.Close()
	r := auto
	r.LLM = &routerconfig.LLM{Endpoint: srv.URL, Model: "m"}
	for _, k := range []string{"Bearer abc", ""} {
		_, _, _ = (&LLM{HTTP: srv.Client()}).Classify(t.Context(), r, k, "x")
	}
	if len(seen) != 2 || seen[0] != "Bearer abc" || seen[1] != "" {
		t.Fatalf("%q", seen)
	}
}

func TestLLMErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		err    bool
	}{"401": {401, `{"error":"bad key sk-secret"}`, true}, "not json": {200, `<html>`, true}, "no choices": {200, `{"choices":[]}`, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		r := auto
		r.LLM = &routerconfig.LLM{Endpoint: srv.URL, Model: "m"}
		ans, _, err := (&LLM{HTTP: srv.Client()}).Classify(t.Context(), r, "sk-secret", "x")
		if (err != nil) != tc.err || ans != "" || (err != nil && strings.Contains(err.Error(), "sk-secret")) {
			t.Errorf("%s: %q %v", name, ans, err)
		}
		srv.Close()
	}
}

func TestKeyNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte("bad key sk-secret"))
	}))
	defer srv.Close()
	r := auto
	r.LLM = &routerconfig.LLM{Endpoint: srv.URL, Model: "m"}
	_, _, err := (&LLM{HTTP: srv.Client()}).Classify(t.Context(), r, "sk-secret", "x")
	if strings.Contains(buf.String(), "sk-secret") || strings.Contains(fmt.Sprint(err), "sk-secret") {
		t.Fatal("key leaked")
	}
}

func TestPrompt(t *testing.T) {
	p := Prompt(auto)
	if !strings.HasPrefix(p, "Classify the user's message") || !strings.Contains(p, "coding: code") || !strings.Contains(p, "other:") || !strings.Contains(p, "exactly one") {
		t.Fatal(p)
	}
}

// A failing classifier is diagnosable from ambit-router's log (wrong URL, revoked key), without the key, and at
// most once per interval per router so a broken classifier cannot flood the log.
func TestClassifierFailureIsLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	cfg := cfgOf(auto)
	h := handler(&fakeLLM{err: fmt.Errorf("classifier answered HTTP 401")}, cfg)
	for i := 0; i < 3; i++ {
		h.decide(t.Context(), time.Now(), h.Store.Snapshot(), Attrs{Groups: []string{"eng"}, Config: cfg.Hash}, body("auto", "code"))
	}
	out := buf.String()
	if strings.Count(out, "classifier call failed") != 1 || !strings.Contains(out, "HTTP 401") || !strings.Contains(out, "router=auto") || strings.Contains(out, "sk") {
		t.Fatalf("want one warning naming the router and the error, without the key:\n%s", out)
	}
}
