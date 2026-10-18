package router

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/craftsman-apps/ambit/internal/routerconfig"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	filterv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

type fakeLLM struct {
	answer   string
	err      error
	delay    time.Duration
	ignore   bool          // ignores ctx (keeps running after the deadline)
	block    chan struct{} // when set, waits for the test to close it
	calls    atomic.Int32
	running  atomic.Int32
	lastText atomic.Value
}

func (f *fakeLLM) Classify(ctx context.Context, _ routerconfig.Router, _, text string) (string, int, error) {
	f.calls.Add(1)
	f.running.Add(1)
	defer f.running.Add(-1)
	f.lastText.Store(text)
	if f.block != nil {
		<-f.block
	}
	if f.ignore {
		time.Sleep(f.delay)
		return f.answer, 8, nil
	}
	select {
	case <-time.After(f.delay):
		return f.answer, 8, f.err
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
}

var auto = routerconfig.Router{Name: "auto", Wire: "auto", Type: "LLM", Groups: []string{"eng"},
	Categories:    []routerconfig.Category{{Name: "coding", Description: "code"}},
	LLM:           &routerconfig.LLM{Endpoint: "http://x", Model: "m", CredentialKey: "local"},
	TimeoutMillis: 200, MaxBytes: 8000, RatePerMinute: 600}

func cfgOf(rs ...routerconfig.Router) routerconfig.Config {
	return routerconfig.Config{Routers: rs, Hash: routerconfig.HashOf(rs)}
}

func body(model, text string) []byte {
	b, _ := json.Marshal(map[string]any{"model": model, "messages": []any{
		map[string]any{"role": "system", "content": "be brief"}, map[string]any{"role": "user", "content": text}}})
	return b
}

func handler(llm Classifier, cfg routerconfig.Config) *Handler {
	s := &Store{}
	s.Set(cfg, map[string]string{"local": "sk"})
	return &Handler{Store: s, Classifier: llm}
}

func TestDecide(t *testing.T) {
	cfg := cfgOf(auto)
	eng := Attrs{Groups: []string{"eng"}, Config: cfg.Hash}
	for name, tc := range map[string]struct {
		llm    *fakeLLM
		attrs  Attrs
		body   []byte
		route  string
		reason string
		calls  int32
	}{
		"classified":         {&fakeLLM{answer: "coding"}, eng, body("auto", "fix my python"), "coding", "classified", 1},
		"answer with spaces": {&fakeLLM{answer: " Coding\n"}, eng, body("auto", "x"), "coding", "classified", 1},
		"extra text":         {&fakeLLM{answer: "coding, because"}, eng, body("auto", "x"), "", "unknown-category", 1},
		"other":              {&fakeLLM{answer: "other"}, eng, body("auto", "x"), "", "unknown-category", 1},
		"empty answer":       {&fakeLLM{answer: ""}, eng, body("auto", "x"), "", "unknown-category", 1},
		"error":              {&fakeLLM{err: errors.New("HTTP 401")}, eng, body("auto", "x"), "", "error", 1},
		"timeout":            {&fakeLLM{delay: time.Second}, eng, body("auto", "x"), "", "timeout", 1},
		"not a router":       {&fakeLLM{answer: "coding"}, eng, body("gpt-4o", "x"), "", "", 0},
		"denied":             {&fakeLLM{answer: "coding"}, Attrs{Groups: []string{"sales"}, Config: cfg.Hash}, body("auto", "x"), "", "denied", 0},
		"no user message":    {&fakeLLM{answer: "coding"}, eng, []byte(`{"model":"auto","messages":[{"role":"system","content":"x"}]}`), "", "error", 0},
		"multi-part":         {&fakeLLM{answer: "coding"}, eng, []byte(`{"model":"auto","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}]}`), "", "error", 0},
		"invalid json":       {&fakeLLM{answer: "coding"}, eng, []byte(`{"model":`), "", "", 0},
	} {
		h := handler(tc.llm, cfg)
		r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), tc.attrs, tc.body)
		if r.Route != tc.route || r.Reason != tc.reason || tc.llm.calls.Load() != tc.calls {
			t.Errorf("%s: got %+v calls %d", name, r, tc.llm.calls.Load())
		}
		if (r.Route != "") != (r.ConfigHash == cfg.Hash) {
			t.Errorf("%s: a label carries the snapshot's hash, and only a label", name)
		}
	}
}

func TestStaleHashGivesStale(t *testing.T) {
	cfg := cfgOf(auto)
	llm := &fakeLLM{answer: "coding"}
	h := handler(llm, cfg)
	for name, tc := range map[string]struct {
		snap *Snapshot
		hash string
	}{"nothing loaded": {nil, cfg.Hash}, "older pod": {h.Store.Snapshot(), "newer-hash"}, "no attribute": {h.Store.Snapshot(), ""}} {
		if r := h.decide(t.Context(), time.Now(), tc.snap, Attrs{Groups: []string{"eng"}, Config: tc.hash}, body("auto", "code")); r.Reason != "stale" || r.Route != "" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if llm.calls.Load() != 0 {
		t.Fatal("stale: no classifier call")
	}
}

func TestDeniedUserCausesNoCall(t *testing.T) {
	llm := &fakeLLM{answer: "coding"}
	cfg := cfgOf(auto)
	h := handler(llm, cfg)
	for _, gs := range [][]string{nil, {}, {"eng-ops"}, {"ENG"}} {
		if r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), Attrs{Groups: gs, Config: cfg.Hash}, body("auto", "code")); r.Reason != "denied" {
			t.Errorf("groups %v: %+v", gs, r)
		}
	}
	open := auto
	open.Groups, open.AllSignedIn = nil, true
	cfg2 := cfgOf(open)
	h2 := handler(llm, cfg2)
	if r := h2.decide(t.Context(), time.Now(), h2.Store.Snapshot(), Attrs{Config: cfg2.Hash}, body("auto", "code")); r.Reason != "classified" {
		t.Errorf("all signed-in: %+v", r)
	}
	if llm.calls.Load() != 1 {
		t.Fatalf("calls %d, want 1", llm.calls.Load())
	}
}

func TestRateLimitedCausesNoCall(t *testing.T) {
	one := auto
	one.RatePerMinute = 1
	cfg := cfgOf(one)
	llm := &fakeLLM{answer: "coding"}
	h := handler(llm, cfg)
	a := Attrs{Groups: []string{"eng"}, Config: cfg.Hash}
	first := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), a, body("auto", "code"))
	second := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), a, body("auto", "code"))
	if first.Reason != "classified" || second.Reason != "rate-limited" || llm.calls.Load() != 1 {
		t.Fatalf("%+v %+v calls %d", first, second, llm.calls.Load())
	}
	h.Store.Set(cfg, nil) // a reload with the same rate keeps the used-up bucket
	if r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), a, body("auto", "code")); r.Reason != "rate-limited" {
		t.Fatalf("after reload: %+v", r)
	}
}

func TestBusyAtOnce(t *testing.T) {
	cfg := cfgOf(auto)
	llm := &fakeLLM{answer: "coding", block: make(chan struct{})}
	h := handler(llm, cfg)
	h.Concurrency = 1
	a := Attrs{Groups: []string{"eng"}, Config: cfg.Hash}
	go h.decide(context.Background(), time.Now().Add(time.Hour), h.Store.Snapshot(), a, body("auto", "code"))
	for llm.running.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	t0 := time.Now()
	r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), a, body("auto", "code"))
	if r.Reason != "busy" || time.Since(t0) > 20*time.Millisecond {
		t.Fatalf("%+v after %v", r, time.Since(t0))
	}
	close(llm.block)
}

// A classifier that ignores cancellation: the request still ends at the deadline (measured from the start of the
// body phase).
func TestHardDeadline(t *testing.T) {
	cfg := cfgOf(auto) // timeout 200 ms
	llm := &fakeLLM{answer: "coding", ignore: true, delay: 2 * time.Second}
	h := handler(llm, cfg)
	start := time.Now().Add(-100 * time.Millisecond) // the body phase started 100 ms ago
	r := h.decide(t.Context(), start, h.Store.Snapshot(), Attrs{Groups: []string{"eng"}, Config: cfg.Hash}, body("auto", "code"))
	if r.Reason != "timeout" || time.Since(start) > 250*time.Millisecond+30*time.Millisecond {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}
}

// ... and its slot stays taken until the worker returns, so a stuck classifier cannot pile up calls.
func TestSlotHeldUntilWorkerReturns(t *testing.T) {
	cfg := cfgOf(auto)
	llm := &fakeLLM{answer: "coding", ignore: true, delay: 500 * time.Millisecond}
	h := handler(llm, cfg)
	h.Concurrency = 1
	a := Attrs{Groups: []string{"eng"}, Config: cfg.Hash}
	if r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), a, body("auto", "code")); r.Reason != "timeout" {
		t.Fatalf("%+v", r)
	}
	if r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), a, body("auto", "code")); r.Reason != "busy" {
		t.Fatalf("the abandoned worker still runs, so the slot is taken: %+v", r)
	}
	time.Sleep(400 * time.Millisecond)
	if r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), a, body("auto", "code")); r.Reason == "busy" {
		t.Fatal("slot must be free once the worker returned")
	}
}

func TestCutOnUTF8Boundary(t *testing.T) {
	five := auto
	five.MaxBytes = 5
	cfg := cfgOf(five)
	llm := &fakeLLM{answer: "coding"}
	h := handler(llm, cfg)
	h.decide(t.Context(), time.Now(), h.Store.Snapshot(), Attrs{Groups: []string{"eng"}, Config: cfg.Hash}, body("auto", "abcd😀"))
	if got := llm.lastText.Load().(string); got != "abcd" || !utf8.ValidString(got) {
		t.Fatalf("%q", got)
	}
}

func TestTwoMiBBody(t *testing.T) {
	cfg := cfgOf(auto)
	llm := &fakeLLM{answer: "coding"}
	h := handler(llm, cfg)
	r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), Attrs{Groups: []string{"eng"}, Config: cfg.Hash}, body("auto", strings.Repeat("a", 2<<20)))
	if r.Reason != "classified" || len(llm.lastText.Load().(string)) != 8000 {
		t.Fatalf("%+v", r)
	}
}

func TestReloadDuringClassificationKeepsSnapshot(t *testing.T) {
	cfg := cfgOf(auto)
	llm := &fakeLLM{answer: "coding", block: make(chan struct{})}
	h := handler(llm, cfg)
	done := make(chan Result)
	snap := h.Store.Snapshot()
	go func() {
		done <- h.decide(context.Background(), time.Now(), snap, Attrs{Groups: []string{"eng"}, Config: cfg.Hash}, body("auto", "code"))
	}()
	for llm.running.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	changed := auto
	changed.Categories = []routerconfig.Category{{Name: "maths", Description: "maths"}}
	h.Store.Set(cfgOf(changed), nil)
	close(llm.block)
	if r := <-done; r.Route != "coding" || r.ConfigHash != cfg.Hash {
		t.Fatalf("the request keeps its snapshot: %+v", r)
	}
}

func stream(t *testing.T, h *Handler) extproc.ExternalProcessor_ProcessClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	extproc.RegisterExternalProcessorServer(srv, h)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	s, err := extproc.NewExternalProcessorClient(conn).Process(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func attrs(groups []string, hash string) map[string]*structpb.Struct {
	gs := []any{}
	for _, g := range groups {
		gs = append(gs, g)
	}
	st, _ := structpb.NewStruct(map[string]any{"ambit.groups": gs, "ambit.config": hash})
	return map[string]*structpb.Struct{"envoy.filters.http.ext_proc": st}
}

func headersOf(r *extproc.ProcessingResponse) map[string]string {
	out := map[string]string{}
	for _, h := range r.GetRequestBody().GetResponse().GetHeaderMutation().GetSetHeaders() {
		out[h.GetHeader().GetKey()] = string(h.GetHeader().GetRawValue())
	}
	return out
}

func exchange(t *testing.T, h *Handler, hash string, reqHeaders []*corev3.HeaderValue, b []byte) (*extproc.ProcessingResponse, *extproc.ProcessingResponse) {
	t.Helper()
	s := stream(t, h)
	if err := s.Send(&extproc.ProcessingRequest{Attributes: attrs([]string{"eng"}, hash), Request: &extproc.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extproc.HttpHeaders{Headers: &corev3.HeaderMap{Headers: reqHeaders}}}}); err != nil {
		t.Fatal(err)
	}
	r1, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(&extproc.ProcessingRequest{Request: &extproc.ProcessingRequest_RequestBody{RequestBody: &extproc.HttpBody{Body: b, EndOfStream: true}}}); err != nil {
		t.Fatal(err)
	}
	r2, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	return r1, r2
}

func TestStreamIgnoresClientHeader(t *testing.T) {
	cfg := cfgOf(auto)
	h := handler(&fakeLLM{answer: "other"}, cfg)
	r1, r2 := exchange(t, h, cfg.Hash, []*corev3.HeaderValue{{Key: "x-ambit-route", RawValue: []byte("coding")}}, body("auto", "x"))
	if r1.GetModeOverride().GetRequestBodyMode() != filterv3.ProcessingMode_BUFFERED {
		t.Fatal("header phase must ask for the buffered body")
	}
	hs := headersOf(r2)
	if hs["x-ambit-route"] != "" || hs["x-ambit-route-reason"] != "unknown-category" || r2.GetRequestBody().GetResponse().GetBodyMutation() != nil {
		t.Fatalf("%v", hs)
	}
}

func TestStreamSetsLabelAndHash(t *testing.T) {
	cfg := cfgOf(auto)
	h := handler(&fakeLLM{answer: "coding"}, cfg)
	_, r2 := exchange(t, h, cfg.Hash, nil, body("auto", "fix this code"))
	hs := headersOf(r2)
	if hs["x-ambit-route"] != "coding" || hs["x-ambit-config"] != cfg.Hash || hs["x-ambit-route-reason"] != "classified" {
		t.Fatalf("%v", hs)
	}
}

func TestStreamNotARouterSetsNothing(t *testing.T) {
	cfg := cfgOf(auto)
	h := handler(&fakeLLM{answer: "coding"}, cfg)
	_, r2 := exchange(t, h, cfg.Hash, nil, body("gpt-4o", "fix this code"))
	if hs := headersOf(r2); len(hs) != 0 {
		t.Fatalf("a request for another model is left alone: %v", hs)
	}
}

// A router granted to named people: the caller's canonical email (from the gateway's ambit.email attribute) must
// equal a listed user exactly; an emailless caller ("") and a subject never match.
func TestUsersMatchByEmail(t *testing.T) {
	llm := &fakeLLM{answer: "coding"}
	pilot := auto
	pilot.Groups, pilot.Users = nil, []string{"alice@acme.com"}
	cfg := cfgOf(pilot)
	h := handler(llm, cfg)
	attrs := func(email string) Attrs {
		st, _ := structpb.NewStruct(map[string]any{"ambit.groups": []any{}, "ambit.email": email, "ambit.config": cfg.Hash})
		return attrsOf(st)
	}
	for email, want := range map[string]string{
		" Alice@Acme.com ": "classified",
		"bob@acme.com":     "denied",
		"":                 "denied",
		"alice":            "denied", // a subject, not an email
		"alice@acme.co":    "denied", // exact, no prefix or domain match
	} {
		if r := h.decide(t.Context(), time.Now(), h.Store.Snapshot(), attrs(email), body("auto", "code")); r.Reason != want {
			t.Errorf("email %q: %+v, want %s", email, r, want)
		}
	}
	if llm.calls.Load() != 1 {
		t.Fatalf("calls %d, want 1", llm.calls.Load())
	}
}
