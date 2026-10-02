package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

const sampleList = `{"_ambit": {"commit": "abc", "date": "2026-10-02"},
 "gpt-4.1": {"litellm_provider": "openai", "input_cost_per_token": 2e-06, "output_cost_per_token": 8e-06},
 "claude-x": {"litellm_provider": "anthropic", "input_cost_per_token": 3e-06, "output_cost_per_token": 1.5e-05},
 "openrouter/deepseek/deepseek-v4-flash": {"litellm_provider": "openrouter", "input_cost_per_token": 4.186e-08, "output_cost_per_token": 8.372e-08},
 "vertex_ai/gemini-x": {"litellm_provider": "vertex_ai-language-models", "input_cost_per_token": 1e-07, "output_cost_per_token": 4e-07},
 "azure/gpt-4.1": {"litellm_provider": "azure", "input_cost_per_token": 2.2e-06, "output_cost_per_token": 8.8e-06}}`

func model(provider, name, baseURL string) v1.Model {
	return v1.Model{Spec: v1.ModelSpec{Provider: provider, Model: name, BaseURL: baseURL}}
}

func TestSuggest(t *testing.T) {
	l, err := ParseLiteLLM([]byte(sampleList), "built in", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		m            v1.Model
		in, out, key string
	}{
		{model("OpenAI", "gpt-4.1", ""), "2", "8", "gpt-4.1"},
		{model("Anthropic", "claude-x", ""), "3", "15", "claude-x"},
		{model("Openrouter", "deepseek/deepseek-v4-flash", ""), "0.04186", "0.08372", "openrouter/deepseek/deepseek-v4-flash"},
		{model("VertexAI", "gemini-x", ""), "0.1", "0.4", "vertex_ai/gemini-x"},
		{model("Azure", "gpt-4.1", ""), "2.2", "8.8", "azure/gpt-4.1"},
	} {
		s, ok := l.Suggest(tc.m)
		if !ok || s.Input != tc.in || s.Output != tc.out || s.Key != tc.key || s.Commit != "abc" || s.Date != "2026-10-02" {
			t.Errorf("%s/%s: %+v %v", tc.m.Spec.Provider, tc.m.Spec.Model, s, ok)
		}
	}
	for _, m := range []v1.Model{model("Anthropic", "gpt-4.1", ""), model("OpenAI", "gpt-4.1", "http://vllm.local/v1"), model("OpenAI", "unknown", "")} {
		if s, ok := l.Suggest(m); ok {
			t.Errorf("suggested for %+v: %+v", m.Spec, s)
		}
	}
}

func TestMirrorRejectsBadPrices(t *testing.T) {
	for _, bad := range []string{`{"x": {"litellm_provider": "openai", "input_cost_per_token": -1, "output_cost_per_token": 1}}`,
		`{"x": {"litellm_provider": "openai", "input_cost_per_token": "1", "output_cost_per_token": 1}}`, `[1,2]`, `not json`,
		// a row that is not an object would silently lose that model's suggestion
		`{"gpt-4.1": {"litellm_provider": "openai", "input_cost_per_token": 2e-06, "output_cost_per_token": 8e-06}, "claude-x": [3e-06, 1.5e-05]}`} {
		if _, err := ParseLiteLLM([]byte(bad), "mirror", "", ""); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// LiteLLM's own file starts with "sample_spec", a description of the fields; it is not a model.
func TestSampleSpecIsNotAModel(t *testing.T) {
	l, err := ParseLiteLLM([]byte(`{"sample_spec": {"input_cost_per_token": 0.0, "litellm_provider": "one of https://docs.litellm.ai/docs/providers"},
 "gpt-4.1": {"litellm_provider": "openai", "input_cost_per_token": 2e-06, "output_cost_per_token": 8e-06}}`), "mirror", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Suggest(model("OpenAI", "gpt-4.1", "")); !ok {
		t.Fatal("no suggestion for gpt-4.1")
	}
}

func TestPriceSourceMirror(t *testing.T) {
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sampleList)) }))
	defer good.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, good.URL, http.StatusFound) }))
	defer redirect.Close()
	big := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat(" ", 11<<20))) }))
	defer big.Close()
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(sampleList)) }))
	defer plain.Close()
	gz := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write([]byte("x"))
	}))
	defer gz.Close()

	ps := &PriceSource{URL: good.URL, HTTP: good.Client()}
	ps.refresh(t.Context())
	if l, err := ps.Current(); err != "" || !strings.HasPrefix(l.Source, "https://") {
		t.Fatalf("good mirror: %q %+v", err, l)
	}
	for name, src := range map[string]*PriceSource{
		"redirect":   {URL: redirect.URL, HTTP: redirect.Client()},
		"too big":    {URL: big.URL, HTTP: big.Client()},
		"timeout":    {URL: slow.URL, HTTP: slow.Client(), Timeout: 100 * time.Millisecond},
		"plain http": {URL: plain.URL, HTTP: plain.Client()},
		"compressed": {URL: gz.URL, HTTP: gz.Client()},
	} {
		src.refresh(t.Context())
		l, err := src.Current()
		if err == "" || l.Source != "built in" {
			t.Errorf("%s: kept %q, error %q", name, l.Source, err)
		}
	}
}

// A failed refresh keeps the last good mirror list, not the built-in one.
func TestFailedRefreshKeepsPreviousMirror(t *testing.T) {
	body := sampleList
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
	defer srv.Close()
	ps := &PriceSource{URL: srv.URL, HTTP: srv.Client()}
	ps.refresh(t.Context())
	body = `{"x": {"litellm_provider": "openai", "input_cost_per_token": -1, "output_cost_per_token": 1}}`
	ps.refresh(t.Context())
	l, err := ps.Current()
	if err == "" || l.Source != srv.URL {
		t.Fatalf("kept %q, error %q", l.Source, err)
	}
}

func TestBuiltinPricesAndLicence(t *testing.T) {
	l := BuiltinPrices()
	if l.Source != "built in" || l.Commit == "" || l.Date == "" || len(l.entries) < 100 {
		t.Fatalf("%s %s %s %d", l.Source, l.Commit, l.Date, len(l.entries))
	}
	if !strings.Contains(LiteLLMLicence, "MIT License") || !strings.Contains(LiteLLMLicence, "Permission is hereby granted") {
		t.Fatal("licence text missing")
	}
}
