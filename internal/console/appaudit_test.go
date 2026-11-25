package console

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/otlplog"
	"github.com/prometheus/client_golang/prometheus/testutil"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// otlpSink is an OTLP logs server that keeps the records it receives (or fails every call).
type otlpSink struct {
	collogs.UnimplementedLogsServiceServer
	mu   sync.Mutex
	got  []map[string]string // attributes of each record, plus "@time" and "@severity"
	fail bool
	hang chan struct{} // when set, Export waits until it is closed
	rec  *AppRecorder  // flushed before the sink is read: the recorder sends from a queue
}

func (f *otlpSink) Export(_ context.Context, r *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	if f.hang != nil {
		<-f.hang
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return nil, grpcstatus.Error(codes.Unavailable, "down")
	}
	for _, rl := range r.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				m := map[string]string{"@time": time.Unix(0, int64(lr.TimeUnixNano)).UTC().Format(time.RFC3339), "@severity": lr.SeverityText,
					"@service": rl.Resource.Attributes[0].Value.GetStringValue(), "@body": lr.Body.GetStringValue()}
				for _, kv := range lr.Attributes {
					m[kv.Key] = kv.Value.GetStringValue()
				}
				f.got = append(f.got, m)
			}
		}
	}
	return &collogs.ExportLogsServiceResponse{}, nil
}

func (f *otlpSink) actions() []string {
	f.rec.Flush()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.got {
		out = append(out, m["ambit.app.action"])
	}
	return out
}

func (f *otlpSink) last() map[string]string {
	f.rec.Flush()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[len(f.got)-1]
}

// appAuditFixture is serversFixture with a collector and a scheme that knows Kubernetes Events.
func appAuditFixture(t *testing.T, gitops *GitOps) (http.Handler, client.Client, *Auth, *otlpSink) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink := &otlpSink{}
	gs := grpc.NewServer()
	collogs.RegisterLogsServiceServer(gs, sink)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	rec, err := NewAppRecorder(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sink.rec = rec
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	timeSrv := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "time", Namespace: testNS},
		Spec:   v1.ServerSpec{Image: timeDigest, Transport: "stdio"},
		Status: v1.ServerStatus{Tools: []v1.Tool{{Name: "get_current_time", Fingerprint: "new"}}, ApprovedTools: []v1.ToolPin{{Name: "get_current_time", Fingerprint: "old"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(timeSrv).
		WithStatusSubresource(&v1.Server{}, &v1.ServerRequest{}).Build()
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	return NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS, GitOps: gitops, AppAudit: rec}), c, auth, sink
}

func eventReasons(t *testing.T, c client.Client, object string) []string {
	t.Helper()
	var l corev1.EventList
	if err := c.List(t.Context(), &l, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range l.Items {
		if e.InvolvedObject.Name == object {
			out = append(out, e.Reason)
		}
	}
	slices.Sort(out)
	return out
}

// Request → refused attempts → approve: each step is one record with who, role, server and what was approved.
func TestAppRequestAndApprovalAreRecorded(t *testing.T) {
	h, c, a, sink := appAuditFixture(t, nil)
	user01, user02 := user(a, "user01@example.com", RoleViewer), user(a, "user02@example.com", RoleViewer)
	user11 := user(a, "user11@example.com", RoleApprover)

	r := do(t, h, "POST", "/api/server-requests", user01, RequestInput{ServerName: "everything", Image: echoDigest, Reason: "reference server"})
	if r.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", r.Code, r.Body)
	}
	req := decodeAs[RequestView](t, r.Body.String())
	got := sink.last()
	for k, want := range map[string]string{"ambit.kind": "app-approval", "ambit.app.action": "requested", "ambit.actor": "user01@example.com",
		"ambit.actor.role": "viewer", "mcp.target": "everything", "ambit.app.request": req.Name, "ambit.app.requested_by": "user01@example.com",
		"ambit.app.image": echoDigest, "ambit.app.comment": "reference server", "@severity": "INFO", "@service": "ambit-console", "@body": "app-approval"} {
		if got[k] != want {
			t.Errorf("requested: %s = %q, want %q", k, got[k], want)
		}
	}
	if _, ok := got["jwt.email"]; ok {
		t.Error("an app record must not set jwt.email: the Audit calls search would count it as a call")
	}

	// A viewer cannot approve; the requester cannot approve their own request. Both attempts are recorded, without the comment.
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", user02, DecisionInput{Comment: "secret text"}); r.Code != http.StatusForbidden {
		t.Fatalf("viewer approve: %d", r.Code)
	}
	got = sink.last()
	if got["ambit.app.action"] != "approve-refused" || got["ambit.actor"] != "user02@example.com" || got["ambit.actor.role"] != "viewer" ||
		got["ambit.app.refused_reason"] != "role viewer may not decide on server requests" || got["mcp.target"] != "everything" ||
		got["ambit.app.requested_by"] != "user01@example.com" || got["@severity"] != "WARN" || got["ambit.app.comment"] != "" {
		t.Errorf("viewer refusal: %v", got)
	}
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/deny", user02, DecisionInput{}); r.Code != http.StatusForbidden {
		t.Fatalf("viewer deny: %d", r.Code)
	}
	if got := sink.last(); got["ambit.app.action"] != "deny-refused" {
		t.Errorf("viewer deny refusal: %v", got)
	}
	user01a := user(a, "user01@example.com", RoleApprover) // the requester, later made an approver
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", user01a, DecisionInput{}); r.Code != http.StatusForbidden {
		t.Fatalf("self-approve: %d", r.Code)
	}
	if got := sink.last(); got["ambit.app.action"] != "approve-refused" || got["ambit.app.refused_reason"] != "own request" || got["ambit.actor.role"] != "approver" {
		t.Errorf("self-approval refusal: %v", got)
	}

	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", user11, DecisionInput{Comment: "digest checked"}); r.Code != 200 {
		t.Fatalf("approve: %d %s", r.Code, r.Body)
	}
	got = sink.last()
	for k, want := range map[string]string{"ambit.app.action": "approved", "ambit.actor": "user11@example.com", "ambit.actor.role": "approver",
		"mcp.target": "everything", "ambit.app.request": req.Name, "ambit.app.requested_by": "user01@example.com",
		"ambit.app.image": echoDigest, "ambit.app.comment": "digest checked"} {
		if got[k] != want {
			t.Errorf("approved: %s = %q, want %q", k, got[k], want)
		}
	}
	// A second decision is refused: already approved.
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/deny", user11, DecisionInput{}); r.Code != http.StatusConflict {
		t.Fatalf("deny after approve: %d", r.Code)
	}
	if got := sink.last(); got["ambit.app.action"] != "deny-refused" || got["ambit.app.refused_reason"] != "already approved" {
		t.Errorf("second decision: %v", got)
	}
	want := []string{"requested", "approve-refused", "deny-refused", "approve-refused", "approved", "deny-refused"}
	if a := sink.actions(); !slices.Equal(a, want) {
		t.Errorf("actions %v, want %v", a, want)
	}
	// And one Kubernetes Event per step on the request.
	if got, want := eventReasons(t, c, req.Name), []string{"ServerRequestApproveRefused", "ServerRequestApproveRefused", "ServerRequestApproved",
		"ServerRequestDenyRefused", "ServerRequestDenyRefused", "ServerRequested"}; !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

func TestAppDenialAndUnknownRequestAreRecorded(t *testing.T) {
	h, _, a, sink := appAuditFixture(t, nil)
	user01, user11 := user(a, "user01@example.com", RoleViewer), user(a, "user11@example.com", RoleAdmin)
	req := decodeAs[RequestView](t, do(t, h, "POST", "/api/server-requests", user01, RequestInput{ServerName: "jira", URL: "https://mcp.example.com/v1/mcp"}).Body.String())
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/deny", user11, DecisionInput{Comment: "use the official server"}); r.Code != 200 {
		t.Fatalf("deny: %d %s", r.Code, r.Body)
	}
	got := sink.last()
	if got["ambit.app.action"] != "denied" || got["ambit.actor"] != "user11@example.com" || got["ambit.actor.role"] != "admin" ||
		got["ambit.app.url"] != "https://mcp.example.com/v1/mcp" || got["ambit.app.comment"] != "use the official server" || got["ambit.app.image"] != "" {
		t.Errorf("denied: %v", got)
	}
	if r := do(t, h, "POST", "/api/server-requests/nothing-x1/approve", user11, DecisionInput{}); r.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", r.Code)
	}
	if got := sink.last(); got["ambit.app.action"] != "approve-refused" || got["ambit.app.request"] != "nothing-x1" || got["ambit.app.refused_reason"] != "no such request" {
		t.Errorf("unknown request: %v", got)
	}
}

// Tool pinning: an admin's approval of changed tool definitions names the tools; a viewer's attempt is refused.
func TestToolApprovalIsRecorded(t *testing.T) {
	h, c, a, sink := appAuditFixture(t, nil)
	viewer, admin := user(a, "user02@example.com", RoleViewer), user(a, "user10@example.com", RoleAdmin)
	if r := do(t, h, "POST", "/api/servers/time/approve-tools", viewer, struct{}{}); r.Code != http.StatusForbidden {
		t.Fatalf("viewer: %d", r.Code)
	}
	if got := sink.last(); got["ambit.app.action"] != "tools-approve-refused" || got["mcp.target"] != "time" || got["ambit.app.refused_reason"] != "role viewer may not approve tool definitions" {
		t.Errorf("viewer: %v", got)
	}
	if r := do(t, h, "POST", "/api/servers/time/approve-tools", admin, struct{}{}); r.Code != 200 {
		t.Fatalf("admin: %d %s", r.Code, r.Body)
	}
	if got := sink.last(); got["ambit.app.action"] != "tools-approved" || got["ambit.app.tools"] != `["get_current_time"]` || got["ambit.actor.role"] != "admin" {
		t.Errorf("approved: %v", got)
	}
	if r := do(t, h, "POST", "/api/servers/time/approve-tools", admin, struct{}{}); r.Code != http.StatusConflict {
		t.Fatalf("again: %d", r.Code)
	}
	if got := sink.last(); got["ambit.app.refused_reason"] != "no tool is waiting for approval" {
		t.Errorf("nothing waiting: %v", got)
	}
	if got, want := eventReasons(t, c, "time"), []string{"ToolsApproveRefused", "ToolsApproveRefused", "ToolsApproved"}; !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// GitOps: the approval opens a pull request (recorded), and the merge is recorded once, at the merge time.
func TestGitOpsApprovalIsRecordedAtTheMerge(t *testing.T) {
	repo := &stateRepo{states: map[string]PullRequestState{}}
	h, c, a, sink := appAuditFixture(t, &GitOps{Repo: repo, Repository: "acme/config", Branch: "main", Dir: "ambit"})
	user01, user11 := user(a, "user01@example.com", RoleViewer), user(a, "user11@example.com", RoleApprover)
	req := decodeAs[RequestView](t, do(t, h, "POST", "/api/server-requests", user01, RequestInput{ServerName: "everything", Image: echoDigest}).Body.String())
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", user11, DecisionInput{Comment: "ok"}); r.Code != http.StatusAccepted {
		t.Fatalf("approve: %d %s", r.Code, r.Body)
	}
	if got := sink.last(); got["ambit.app.action"] != "pull-request-opened" || got["ambit.app.pull_request"] != "https://github.com/acme/config/pull/1" || got["ambit.app.comment"] != "ok" {
		t.Errorf("opened: %v", got)
	}
	var q v1.ServerRequest
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: req.Name}, &q); err != nil {
		t.Fatal(err)
	}
	merged := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
	repo.states[q.Status.PullRequestBranch] = PullRequestState{Merged: true, MergedAt: merged}
	for range 2 { // two listings: the first settles the decision, the second finds it settled
		do(t, h, "GET", "/api/server-requests", user11, nil)
	}
	want := []string{"requested", "pull-request-opened", "approved"}
	if a := sink.actions(); !slices.Equal(a, want) {
		t.Fatalf("actions %v, want %v", a, want)
	}
	if got := sink.last(); got["ambit.actor"] != "user11@example.com" || got["ambit.actor.role"] != "" || got["@time"] != "2026-11-18T09:30:00Z" ||
		got["ambit.app.pull_request"] != "https://github.com/acme/config/pull/1" {
		t.Errorf("merged: %v", got)
	}
}

// A collector that is down does not fail the decision; the failure is counted for the alert.
func TestAppRecordSendFailureIsCounted(t *testing.T) {
	h, _, a, sink := appAuditFixture(t, nil)
	sink.fail = true
	before := testutil.ToFloat64(appSendFailures.WithLabelValues(otlplog.DropSendFailed))
	if r := do(t, h, "POST", "/api/server-requests", user(a, "user01@example.com", RoleViewer), RequestInput{ServerName: "everything", Image: echoDigest}); r.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", r.Code, r.Body)
	}
	sink.rec.Flush()
	if d := testutil.ToFloat64(appSendFailures.WithLabelValues(otlplog.DropSendFailed)) - before; d != 1 {
		t.Errorf("failures counted: %v, want 1", d)
	}
}

// A collector that does not answer does not delay the decision (issue 331: it used to wait up to 5 seconds).
func TestAppRecordDoesNotWaitForTheCollector(t *testing.T) {
	h, _, a, sink := appAuditFixture(t, nil)
	sink.hang = make(chan struct{})
	t.Cleanup(func() { close(sink.hang) })
	viewer, approver := user(a, "user01@example.com", RoleViewer), user(a, "user11@example.com", RoleApprover)
	start := time.Now()
	req := decodeAs[RequestView](t, do(t, h, "POST", "/api/server-requests", viewer, RequestInput{ServerName: "everything", Image: echoDigest}).Body.String())
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", approver, DecisionInput{}); r.Code != 200 {
		t.Fatalf("approve: %d %s", r.Code, r.Body)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("request and approval took %v with a collector that does not answer", d)
	}
}

func TestAppEventsSQL(t *testing.T) {
	for _, s := range []string{"LogAttributes['ambit.kind'] = 'app-approval'", "lower(LogAttributes['ambit.actor']) = lower({email:String})",
		"lower(LogAttributes['ambit.app.requested_by']) = lower({email:String})", "{email:String} = ''", "ORDER BY Timestamp DESC"} {
		if !strings.Contains(appEventsSQL, s) {
			t.Errorf("appEventsSQL lacks %q", s)
		}
	}
	if strings.Contains(appEventsSQL, "jwt.email") {
		t.Error("appEventsSQL reads jwt.email")
	}
	// The calls search must not match app records: they have no jwt.email and no jwt.sub.
	if !strings.Contains(callsSQL, "LogAttributes['jwt.email'] = {email:String}") {
		t.Error("callsSQL changed its person filter; check that app records stay out of it")
	}
}

func TestAuditApprovalsMode(t *testing.T) {
	a := OpenAudit(ClickHouse{Addr: "127.0.0.1:1", Password: "x"}) // nothing listens here
	mux := http.NewServeMux()
	a.register(mux, func(h func(http.ResponseWriter, *http.Request, Session)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			h(w, r, Session{Email: "user10@example.com", Role: RoleAdmin})
		}
	})
	for u, code := range map[string]int{
		"/api/audit?approvals=yes":                              http.StatusBadRequest,
		"/api/audit?approvals=true&invalidKey=true":             http.StatusBadRequest,
		"/api/audit?approvals=true&from=nope":                   http.StatusBadRequest,
		"/api/audit?approvals=true":                             http.StatusBadGateway, // reaches the store
		"/api/audit?approvals=true&email=user01%40example.com":  http.StatusBadGateway,
		"/api/audit?email=user01%40example.com&invalidKey=true": http.StatusBadRequest, // unchanged
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", u, nil))
		if rec.Code != code {
			t.Errorf("%s: %d %s, want %d", u, rec.Code, rec.Body.String(), code)
		}
	}
}
