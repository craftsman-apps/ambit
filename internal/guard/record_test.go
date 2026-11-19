package guard

import (
	"context"
	"net"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/extmcp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeCollector is an OTLP logs server that keeps what it receives (or fails every call).
type fakeCollector struct {
	collogs.UnimplementedLogsServiceServer
	mu   sync.Mutex
	got  []*collogs.ExportLogsServiceRequest
	fail bool
}

func (f *fakeCollector) Export(_ context.Context, r *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, status.Error(codes.Unavailable, "down")
	}
	f.got = append(f.got, r)
	return &collogs.ExportLogsServiceResponse{}, nil
}

// attrs flattens every received record to a map of its attributes.
func (f *fakeCollector) attrs() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]string
	for _, r := range f.got {
		for _, rl := range r.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					m := map[string]string{}
					for _, kv := range lr.Attributes {
						m[kv.Key] = kv.Value.GetStringValue()
					}
					out = append(out, m)
				}
			}
		}
	}
	return out
}

func startCollector(t *testing.T) (*fakeCollector, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCollector{}
	s := grpc.NewServer()
	collogs.RegisterLogsServiceServer(s, f)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return f, lis.Addr().String()
}

func groupsMeta(email string, groups ...string) *structpb.Struct {
	vals := make([]any, len(groups))
	for i, g := range groups {
		vals[i] = g
	}
	m := map[string]any{"groups": vals}
	if email != "" {
		m["email"] = email
	}
	s, _ := structpb.NewStruct(m)
	return s
}

func callAs(t testing.TB, g *Guard, meta *structpb.Struct, body []byte) {
	t.Helper()
	if _, err := g.CheckResponse(t.Context(), &extmcp.McpResponse{ServiceNames: []string{"crm"}, Method: "tools/call", McpResponse: body, MetadataContext: meta}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

type sink struct {
	mu   sync.Mutex
	recs []CompressionRecord
}

func (s *sink) Record(r CompressionRecord) { s.mu.Lock(); s.recs = append(s.recs, r); s.mu.Unlock() }

// One record per compressed text, with the caller's groups and sizes; none for texts that stay as they are.
func TestRecordPerCompressedText(t *testing.T) {
	s := &sink{}
	g := &Guard{Masker: Builtin{}, Compression: on(ModeLossless), Records: s}
	rows := issueList(100)
	body := mustJSON(map[string]any{"content": []map[string]any{
		{"type": "text", "text": rows}, {"type": "text", "text": rows}, {"type": "text", "text": "short"},
		{"type": "text", "text": strings.Repeat("The quick brown fox jumps over the lazy dog. ", 400)}, // prose: no_gain
	}})
	callAs(t, g, groupsMeta("user05@acme.example", "eng", "ml-research"), body)
	if len(s.recs) != 2 {
		t.Fatalf("want 2 records (two compressed texts), got %d", len(s.recs))
	}
	r := s.recs[0]
	if r.Server != "crm" || r.Type != "json" || r.Mode != ModeLossless || r.BytesOut >= r.BytesIn || r.BytesIn != len(rows) {
		t.Errorf("bad record: %+v", r)
	}
	if strings.Join(r.Groups, ",") != "eng,ml-research" {
		t.Errorf("groups: %v", r.Groups)
	}
}

// No groups key: nothing to count it in, so it is dropped and counted. An empty list is kept (the view files it as "(no group)").
func TestRecordGroupsRules(t *testing.T) {
	s := &sink{}
	g := &Guard{Masker: Builtin{}, Compression: on(ModeLossless), Records: s}
	body := mustJSON(map[string]any{"content": []map[string]any{{"type": "text", "text": issueList(100)}}})
	before := testutil.ToFloat64(recordDropped.WithLabelValues(dropNoGroups))
	callAs(t, g, nil, body)
	callAs(t, g, &structpb.Struct{}, body)
	if len(s.recs) != 0 || testutil.ToFloat64(recordDropped.WithLabelValues(dropNoGroups))-before != 2 {
		t.Errorf("missing groups: records=%d dropped=%v", len(s.recs), testutil.ToFloat64(recordDropped.WithLabelValues(dropNoGroups))-before)
	}
	callAs(t, g, groupsMeta("user05@acme.example"), body)
	if len(s.recs) != 1 || len(s.recs[0].Groups) != 0 {
		t.Errorf("empty groups: %+v", s.recs)
	}
}

// The compressed result still reaches the caller when records are off or the sink is slow.
func TestRecordDoesNotChangeTheResult(t *testing.T) {
	body := mustJSON(map[string]any{"content": []map[string]any{{"type": "text", "text": issueList(100)}}})
	meta := groupsMeta("user05@acme.example", "eng")
	run := func(g *Guard) string {
		res, _ := g.CheckResponse(t.Context(), &extmcp.McpResponse{ServiceNames: []string{"crm"}, Method: "tools/call", McpResponse: body, MetadataContext: meta})
		return textOf(t, res)
	}
	if run(&Guard{Masker: Builtin{}, Compression: on(ModeLossless)}) != run(&Guard{Masker: Builtin{}, Compression: on(ModeLossless), Records: &sink{}}) {
		t.Error("records changed the result")
	}
}

// callMeta is the guard policy's response metadata with protection.compressionAudit.caller true: the caller as the
// access log writes it (recordEmail, keyId) and the call from CheckRequest (callId, tool).
func callMeta(callID, keyID string, groups ...string) *structpb.Struct {
	m := groupsMeta("", groups...).AsMap()
	m["email"], m["recordEmail"], m["keyId"], m["callId"], m["tool"] = "", "user05@acme.example", keyID, callID, "list_runs"
	s, _ := structpb.NewStruct(m)
	return s
}

// #203: the record names the caller and the call, with exactly the attributes of the spec (section 2). The guard's
// own argument-limit email (empty here: not verified) is not used; the record takes recordEmail.
func TestRecordNamesTheCallerAndTheCall(t *testing.T) {
	f, addr := startCollector(t)
	rec, err := NewRecorder(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	g := &Guard{Masker: Builtin{}, Compression: on(ModeShorten), Records: rec}
	callAs(t, g, callMeta("c_0123456789abcdef0123456789abcdef", "k_7Hq", "eng"),
		mustJSON(map[string]any{"content": []map[string]any{{"type": "text", "text": "short"}, {"type": "text", "text": issueList(100)}}}))
	waitFor(t, "the record", func() bool { return len(f.attrs()) == 1 })
	a := f.attrs()[0]
	want := map[string]string{"ambit.kind": "compression", "mcp.target": "crm", "gen_ai.tool.name": "list_runs", "ambit.compress.type": "json",
		"ambit.compress.mode": "shorten", "ambit.compress.part": "1", "jwt.groups": `["eng"]`, "jwt.email": "user05@acme.example",
		"ambit.key_id": "k_7Hq", "ambit.call_id": "c_0123456789abcdef0123456789abcdef"}
	for _, k := range []string{"ambit.compress.bytes_in", "ambit.compress.bytes_out", "ambit.compress.omitted"} {
		if a[k] == "" {
			t.Errorf("%s missing", k)
		}
		want[k] = a[k]
	}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("attributes:\n got %v\nwant %v", a, want)
	}
}

// A caller with no group (a key owner without groups) is recorded with [] and still names the call; with
// compressionAudit.caller false (no recordEmail, keyId) the record has the call and no person; a record from an older
// guard policy (no callId, tool, recordEmail or keyId) writes none of those attributes, not empty ones.
func TestRecordEmptyGroupsAndOldPolicy(t *testing.T) {
	f, addr := startCollector(t)
	rec, _ := NewRecorder(addr)
	defer rec.Close()
	g := &Guard{Masker: Builtin{}, Compression: on(ModeLossless), Records: rec}
	body := mustJSON(map[string]any{"content": []map[string]any{{"type": "text", "text": issueList(100)}}})
	callAs(t, g, callMeta("c_1", "", []string{}...), body)
	callAs(t, g, groupsMeta("user05@acme.example", "eng"), body)
	off := callMeta("c_2", "k_7Hq", "eng")
	delete(off.Fields, "recordEmail")
	delete(off.Fields, "keyId")
	callAs(t, g, off, body)
	waitFor(t, "three records", func() bool { return len(f.attrs()) == 3 })
	a := f.attrs()
	if a[0]["jwt.groups"] != "[]" || a[0]["ambit.call_id"] != "c_1" || a[0]["jwt.email"] != "user05@acme.example" {
		t.Errorf("no groups: %v", a[0])
	}
	if _, ok := a[0]["ambit.key_id"]; ok {
		t.Errorf("empty key ID written: %v", a[0])
	}
	for _, k := range []string{"ambit.call_id", "gen_ai.tool.name", "jwt.email", "ambit.key_id"} {
		if _, ok := a[1][k]; ok {
			t.Errorf("old policy: %s written: %v", k, a[1])
		}
	}
	if _, ok := a[2]["jwt.email"]; ok || a[2]["ambit.call_id"] != "c_2" || a[2]["gen_ai.tool.name"] != "list_runs" || a[2]["ambit.key_id"] != "" {
		t.Errorf("caller false: %v", a[2])
	}
}

// CheckRequest makes a new call ID for every tools/call and returns it with the tool's own name; a callId in the
// request's metadata (the policy evaluates it before the guard has answered, so it is "") is never used.
func TestCheckRequestMakesTheCallID(t *testing.T) {
	g := &Guard{Pins: pins, Masker: Builtin{}}
	md, _ := structpb.NewStruct(map[string]any{"groups": []any{"eng"}, "callId": "c_chosen_by_someone"})
	seen := map[string]bool{}
	for range 10000 {
		res, _ := g.CheckRequest(t.Context(), &extmcp.McpRequest{ServiceNames: []string{"crm"}, Method: "tools/call", MetadataContext: md,
			McpRequest: []byte(`{"name":"get_customer","arguments":{"callId":"c_chosen_by_someone"}}`)})
		f := res.GetMetadata().GetFields()
		id := f["callId"].GetStringValue()
		if res.GetPass() == nil || f["tool"].GetStringValue() != "get_customer" || !regexp.MustCompile(`^c_[0-9a-f]{32}$`).MatchString(id) || seen[id] {
			t.Fatalf("result %v", res)
		}
		seen[id] = true
	}
	// Other methods get no call ID.
	res, _ := g.CheckRequest(t.Context(), &extmcp.McpRequest{ServiceNames: []string{"crm"}, Method: "tools/list"})
	if res.GetMetadata() != nil {
		t.Errorf("tools/list: %v", res)
	}
}

// End to end over gRPC: the record has sizes, names and groups as attributes, and no content. Without recordEmail
// (protection.compressionAudit.caller false) it has no email, even though the guard's own email metadata is set.
func TestRecorderSendsOTLP(t *testing.T) {
	f, addr := startCollector(t)
	rec, err := NewRecorder(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	g := &Guard{Masker: Builtin{}, Compression: on(ModeShorten), Records: rec}
	callAs(t, g, groupsMeta("user05@acme.example", "eng", "ml-research"), mustJSON(map[string]any{"content": []map[string]any{{"type": "text", "text": issueList(100)}}}))
	waitFor(t, "the record", func() bool { return len(f.attrs()) == 1 })
	a := f.attrs()[0]
	want := map[string]string{"ambit.kind": "compression", "mcp.target": "crm", "ambit.compress.type": "json", "ambit.compress.mode": "shorten", "jwt.groups": `["eng","ml-research"]`}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("%s = %q, want %q", k, a[k], v)
		}
	}
	for _, k := range []string{"ambit.compress.bytes_in", "ambit.compress.bytes_out", "ambit.compress.omitted"} {
		if a[k] == "" {
			t.Errorf("%s missing", k)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.got[0].String(); strings.Contains(s, "user05@acme") || strings.Contains(s, "jwt.email") || strings.Contains(s, "Fix flaky test") {
		t.Errorf("record holds an email or content: %.300s", s)
	}
}

// A full queue drops and counts; Record never blocks.
func TestRecorderFullQueueDrops(t *testing.T) {
	r := &Recorder{queue: make(chan CompressionRecord, 2)}
	before := testutil.ToFloat64(recordDropped.WithLabelValues(dropQueueFull))
	done := make(chan struct{})
	go func() {
		for range 5 {
			r.Record(CompressionRecord{Server: "crm"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked")
	}
	if d := testutil.ToFloat64(recordDropped.WithLabelValues(dropQueueFull)) - before; d != 3 {
		t.Errorf("dropped %v, want 3", d)
	}
}

// A collector that is down or refuses costs the records, counted, and does not fail the call.
func TestRecorderCollectorDown(t *testing.T) {
	f, addr := startCollector(t)
	f.fail = true
	rec, _ := NewRecorder(addr)
	g := &Guard{Masker: Builtin{}, Compression: on(ModeLossless), Records: rec}
	before := testutil.ToFloat64(recordDropped.WithLabelValues(dropSendFailed))
	res, err := g.CheckResponse(t.Context(), &extmcp.McpResponse{ServiceNames: []string{"crm"}, Method: "tools/call", MetadataContext: groupsMeta("", "eng"),
		McpResponse: mustJSON(map[string]any{"content": []map[string]any{{"type": "text", "text": issueList(100)}}})})
	if err != nil || res.GetMutated() == nil {
		t.Fatalf("call failed: %v %v", res, err)
	}
	rec.Close() // flushes, so the failed export has happened
	if d := testutil.ToFloat64(recordDropped.WithLabelValues(dropSendFailed)) - before; d != 1 {
		t.Errorf("send_failed %v, want 1", d)
	}
	// And an address nobody listens on.
	rec2, _ := NewRecorder("127.0.0.1:1")
	rec2.Record(CompressionRecord{Server: "crm"})
	rec2.Close()
}

// A collector that never answers must not slow the callers: Record returns at once, and Close returns within its limit.
func TestRecorderBlockedCollectorDoesNotBlockCallers(t *testing.T) {
	release := make(chan struct{})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	collogs.RegisterLogsServiceServer(s, &blockedCollector{release: release})
	go func() { _ = s.Serve(lis) }()
	defer func() { close(release); s.Stop() }()
	rec, _ := NewRecorder(lis.Addr().String())
	start := time.Now()
	for range 5 * recordQueue {
		rec.Record(CompressionRecord{Server: "crm", Groups: []string{"eng"}})
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("%d Record calls took %v with a collector that does not answer", 5*recordQueue, d)
	}
	closed := make(chan struct{})
	go func() { rec.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(recordTimeout + 3*time.Second):
		t.Fatal("Close did not return")
	}
}

type blockedCollector struct {
	collogs.UnimplementedLogsServiceServer
	release chan struct{}
}

func (b *blockedCollector) Export(ctx context.Context, _ *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return &collogs.ExportLogsServiceResponse{}, nil
}
