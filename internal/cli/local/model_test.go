package local

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// A recorded excerpt of https://openrouter.ai/api/v1/models (2026-10-14), plus one paid model.
const recordedList = `{"data":[
 {"id":"openai/gpt-5","context_length":400000},
 {"id":"thinkingmachines/inkling:free","context_length":1048576},
 {"id":"google/gemma-4-31b-it:free","context_length":262144},
 {"id":"liquid/lfm-2.5-2.6b:free","context_length":65536},
 {"id":"nvidia/nemotron-3-super-120b-a12b:free","context_length":262144}]}`

func serve(t *testing.T, body string, code int) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func TestFetchFreeModels(t *testing.T) {
	free, err := FetchFreeModels(context.Background(), http.DefaultClient, serve(t, recordedList, 200))
	if err != nil || len(free) != 4 {
		t.Fatalf("free = %v, %v", free, err)
	}
	if _, err := FetchFreeModels(context.Background(), http.DefaultClient, serve(t, "", 503)); err == nil {
		t.Fatal("HTTP 503 must be an error")
	}
}

func TestCandidates(t *testing.T) {
	free, _ := FetchFreeModels(context.Background(), http.DefaultClient, serve(t, recordedList, 200))
	got, err := Candidates(free, "")
	want := []string{"nvidia/nemotron-3-super-120b-a12b:free", "google/gemma-4-31b-it:free", "thinkingmachines/inkling:free"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Candidates = %v, %v; want %v", got, err, want)
	}
	// No preferred model present: largest context window first.
	got, _ = Candidates([]FreeModel{{"a:free", 1000}, {"b:free", 9000}}, "")
	if !reflect.DeepEqual(got, []string{"b:free", "a:free"}) {
		t.Fatalf("no preferred: %v", got)
	}
	if _, err := Candidates(nil, ""); err == nil || err.Error() != "OpenRouter lists no free model" {
		t.Fatalf("empty list: %v", err)
	}
	if got, _ := Candidates(nil, "x-ai/grok-5:free"); !reflect.DeepEqual(got, []string{"x-ai/grok-5:free"}) {
		t.Fatalf("--model: %v", got)
	}
}

func TestTryCandidates(t *testing.T) {
	id, err := TryCandidates([]string{"a", "b", "c"}, func(id string) error {
		if id == "b" {
			return nil
		}
		return errors.New("HTTP 429")
	})
	if err != nil || id != "b" {
		t.Fatalf("= %q, %v", id, err)
	}
	_, err = TryCandidates([]string{"a", "b", "c"}, func(string) error { return errors.New("HTTP 429") })
	if err == nil || !strings.Contains(err.Error(), "a: HTTP 429; b: HTTP 429; c: HTTP 429") {
		t.Fatalf("all failing: %v", err)
	}
}
