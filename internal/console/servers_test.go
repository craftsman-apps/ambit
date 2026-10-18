package console

import (
	"encoding/json"
	"github.com/craftsman-apps/ambit/internal/identity"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	timeDigest = "ghcr.io/stacklok/dockyard/uvx/mcp-server-time@sha256:ab8255cf7bf2bdad14086b356790d2d61d06cab221b85950805427be9d06d949"
	echoDigest = "ghcr.io/stacklok/dockyard/npx/server-everything@sha256:cb2e2287d4b6a7d25affee2cde10265bfbf91f9dc38d5cbc8165c6b23fd0cded"
)

func serversFixture(t *testing.T) (http.Handler, client.Client, *Auth) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	timeSrv := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "time", Namespace: testNS},
		Spec: v1.ServerSpec{Image: timeDigest, Transport: "stdio", DisplayName: "Time"},
		Status: v1.ServerStatus{Tools: []v1.Tool{{Name: "convert_time"}, {Name: "get_current_time"}},
			Conditions: []metav1.Condition{{Type: "Ready", Status: "True", Reason: "Running", Message: "running"}}}}
	engTime := &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng-time", Namespace: testNS},
		Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Servers: []v1.ServerAccess{{Name: "time", Tools: []string{"get_current_time"}}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(timeSrv, engTime).
		WithStatusSubresource(&v1.Model{}, &v1.AccessRule{}, &v1.Server{}, &v1.ServerRequest{}).Build()
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	return NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS}), c, auth
}

func user(a *Auth, email, role string, groups ...string) *http.Cookie {
	s := Session{ID: randomString(), Email: email, Role: role, Groups: groups, Exp: time.Now().Add(time.Hour).Unix()}
	a.Sessions.Add(s.ID, s.Email, s.Exp)
	return &http.Cookie{Name: SessionCookie, Value: a.codec.Encode(s)}
}

func decodeAs[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

func TestRequestApproveCreatesServer(t *testing.T) {
	h, c, a := serversFixture(t)
	alice := user(a, "alice@acme.test", RoleViewer, "eng")
	sam := user(a, "sam@acme.test", RoleApprover)

	r := do(t, h, "POST", "/api/server-requests", alice, RequestInput{ServerName: "everything", Image: echoDigest, Reason: "reference server for tests"})
	if r.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", r.Code, r.Body)
	}
	req := decodeAs[RequestView](t, r.Body.String())
	if req.RequestedBy != "alice@acme.test" || req.Transport != "stdio" || req.Decision != "" || !strings.HasPrefix(req.Name, "everything-") {
		t.Fatalf("request view %+v", req)
	}
	// Nothing runs before approval.
	var srv v1.Server
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "everything"}, &srv); err == nil {
		t.Fatal("server exists before approval")
	}
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", alice, DecisionInput{}); r.Code != http.StatusForbidden {
		t.Fatalf("viewer approved: %d", r.Code)
	}
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", sam, DecisionInput{Comment: "digest checked"}); r.Code != 200 {
		t.Fatalf("approve: %d %s", r.Code, r.Body)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "everything"}, &srv); err != nil {
		t.Fatal(err)
	}
	if srv.Spec.Image != echoDigest || srv.Annotations[approvedByKey] != "sam@acme.test" || srv.Annotations[requestedByKey] != "alice@acme.test" || srv.Annotations[requestKey] != req.Name {
		t.Fatalf("server %+v %v", srv.Spec, srv.Annotations)
	}
	var q v1.ServerRequest
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: req.Name}, &q); err != nil {
		t.Fatal(err)
	}
	if q.Status.Decision != "Approved" || q.Status.DecidedBy != "sam@acme.test" || q.Status.Comment != "digest checked" || q.Status.DecidedAt == nil {
		t.Fatalf("status %+v", q.Status)
	}
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/deny", sam, DecisionInput{}); r.Code != http.StatusConflict {
		t.Fatalf("second decision: %d", r.Code)
	}
}

func TestNobodyApprovesTheirOwnRequest(t *testing.T) {
	h, c, a := serversFixture(t)
	priya := user(a, "priya@acme.test", RoleAdmin, "ambit-admins")
	r := do(t, h, "POST", "/api/server-requests", priya, RequestInput{ServerName: "everything", Image: echoDigest})
	req := decodeAs[RequestView](t, r.Body.String())
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", priya, DecisionInput{}); r.Code != http.StatusForbidden {
		t.Fatalf("admin approved own request: %d %s", r.Code, r.Body)
	}
	var srv v1.Server
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "everything"}, &srv); err == nil {
		t.Fatal("server created")
	}
}

func TestDenyLeavesNoServer(t *testing.T) {
	h, c, a := serversFixture(t)
	r := do(t, h, "POST", "/api/server-requests", user(a, "alice@acme.test", RoleViewer), RequestInput{ServerName: "everything", Image: echoDigest})
	req := decodeAs[RequestView](t, r.Body.String())
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/deny", user(a, "sam@acme.test", RoleApprover), DecisionInput{Comment: "use the internal fork"}); r.Code != 200 {
		t.Fatalf("deny: %d %s", r.Code, r.Body)
	}
	var srv v1.Server
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "everything"}, &srv); err == nil {
		t.Fatal("denied request created a server")
	}
	list := decodeAs[[]RequestView](t, do(t, h, "GET", "/api/server-requests", user(a, "x@acme.test", RoleViewer), nil).Body.String())
	if len(list) != 1 || list[0].Decision != "Denied" || list[0].Comment != "use the internal fork" {
		t.Fatalf("history %+v", list)
	}
}

func TestInvalidRequestsAreRefusedWithReason(t *testing.T) {
	h, _, a := serversFixture(t)
	alice := user(a, "alice@acme.test", RoleViewer)
	for _, tc := range []struct {
		in   RequestInput
		want string
	}{
		{RequestInput{ServerName: "everything", Image: "ghcr.io/stacklok/dockyard/npx/server-everything:2026.8.31"}, "pin the image by digest"},
		{RequestInput{ServerName: "Everything", Image: echoDigest}, "lowercase"},
		{RequestInput{ServerName: "everything", Image: echoDigest, Transport: "websocket"}, "transport"},
		{RequestInput{ServerName: "time", Image: timeDigest}, "already exists"},
	} {
		r := do(t, h, "POST", "/api/server-requests", alice, tc.in)
		if r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), tc.want) {
			t.Errorf("%+v: %d %s", tc.in, r.Code, r.Body)
		}
	}
	if r := do(t, h, "POST", "/api/server-requests", alice, RequestInput{ServerName: "everything", Image: echoDigest}); r.Code != http.StatusCreated {
		t.Fatalf("first: %d", r.Code)
	}
	if r := do(t, h, "POST", "/api/server-requests", alice, RequestInput{ServerName: "everything", Image: echoDigest}); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), "already waiting") {
		t.Fatalf("duplicate: %d %s", r.Code, r.Body)
	}
}

func TestServersListShowsToolsAndRules(t *testing.T) {
	h, _, a := serversFixture(t)
	list := decodeAs[[]ServerView](t, do(t, h, "GET", "/api/servers", user(a, "alice@acme.test", RoleViewer), nil).Body.String())
	if len(list) != 1 || list[0].Name != "time" || len(list[0].Tools) != 2 || list[0].Ready != "True" || len(list[0].Rules) != 1 || list[0].Rules[0] != "eng-time" {
		t.Fatalf("%+v", list)
	}
}

func TestAdminEditsToolsInARule(t *testing.T) {
	h, c, a := serversFixture(t)
	admin := user(a, "priya@acme.test", RoleAdmin)
	in := RuleInput{Name: "ml-time", Groups: []string{"ml-research"}, Servers: []v1.ServerAccess{{Name: "time"}}}
	if r := do(t, h, "POST", "/api/access-rules", admin, in); r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	d := decodeAs[RuleDetail](t, do(t, h, "GET", "/api/access-rules/ml-time", admin, nil).Body.String())
	if len(d.EffectiveServers) != 1 || !strings.Contains(d.EffectiveServers[0].CEL, `mcp.tool.target == "time" && `+identity.PrincipalGroups+`.exists(g, g in ["ml-research"])`) {
		t.Fatalf("effective %+v", d.EffectiveServers)
	}
	// Narrow to one tool.
	in.Servers = []v1.ServerAccess{{Name: "time", Tools: []string{"convert_time"}}}
	if r := do(t, h, "PUT", "/api/access-rules/ml-time", admin, in); r.Code != 200 {
		t.Fatalf("update: %d %s", r.Code, r.Body)
	}
	var got v1.AccessRule
	c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "ml-time"}, &got)
	if len(got.Spec.Servers) != 1 || len(got.Spec.Servers[0].Tools) != 1 || got.Spec.Servers[0].Tools[0] != "convert_time" {
		t.Fatalf("spec %+v", got.Spec)
	}
	// Raw JSON: "tools": [] must not be read as "all tools" (Go's omitempty would drop it when encoding).
	for _, bad := range []string{
		`[{"name":"tiem"}]`,
		`[{"name":"time","tools":[]}]`,
		`[{"name":"time","tools":["x\" || true || \""]}]`,
	} {
		body := json.RawMessage(`{"groups":["ml-research"],"allSignedIn":false,"models":[],"servers":` + bad + `}`)
		if r := do(t, h, "PUT", "/api/access-rules/ml-time", admin, body); r.Code != http.StatusBadRequest {
			t.Errorf("%s accepted: %d", bad, r.Code)
		}
	}
}

func TestMyAccessExplainsWhatAUserMayUse(t *testing.T) {
	h, _, a := serversFixture(t)
	got := decodeAs[MyAccess](t, do(t, h, "GET", "/api/me/access", user(a, "alice@acme.test", RoleViewer, "eng"), nil).Body.String())
	if len(got.Servers) != 1 || got.Servers[0].Server != "time" || got.Servers[0].AllTools || len(got.Servers[0].Tools) != 1 ||
		got.Servers[0].Tools[0] != "get_current_time" || got.Servers[0].Rules[0] != "eng-time" {
		t.Fatalf("alice: %+v", got)
	}
	none := decodeAs[MyAccess](t, do(t, h, "GET", "/api/me/access", user(a, "mira@acme.test", RoleViewer, "ml-research"), nil).Body.String())
	if len(none.Servers) != 0 || len(none.Models) != 0 {
		t.Fatalf("mira: %+v", none)
	}
}

// A vendor-hosted MCP server is requested by URL, optionally with a Connection so each call carries
// the caller's own token; approval creates the Server as asked.
func TestRemoteServerRequest(t *testing.T) {
	h, c, a := serversFixture(t)
	if err := c.Create(t.Context(), &v1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "jira", Namespace: testNS},
		Spec: v1.ConnectionSpec{DisplayName: "Jira", AuthorizationURL: "https://auth.example.com/authorize", TokenURL: "https://auth.example.com/token",
			ClientID: "ambit", ClientSecretRef: v1.SecretRef{Name: "ambit-conn-jira"}}}); err != nil {
		t.Fatal(err)
	}
	alice := user(a, "alice@acme.test", RoleViewer, "eng")
	sam := user(a, "sam@acme.test", RoleApprover)
	for _, tc := range []struct {
		in   RequestInput
		want string
	}{
		{RequestInput{ServerName: "jira", URL: "https://mcp.atlassian.example/v1/mcp", Image: echoDigest}, "either an image or a URL"},
		{RequestInput{ServerName: "jira"}, "either an image or a URL"},
		{RequestInput{ServerName: "jira", URL: "http://mcp.atlassian.example/v1/mcp"}, "https"},
		{RequestInput{ServerName: "jira", URL: "https://mcp.atlassian.example/v1/mcp?token=x"}, "URL"},
		{RequestInput{ServerName: "jira", URL: "https://mcp.atlassian.example/v1/mcp", Transport: "stdio"}, "streamable-http or sse"},
		{RequestInput{ServerName: "jira", URL: "https://mcp.atlassian.example/v1/mcp", Connection: "confluence"}, "no connection named"},
		{RequestInput{ServerName: "tool", Image: echoDigest, Connection: "jira"}, "only for servers reached by URL"},
	} {
		if r := do(t, h, "POST", "/api/server-requests", alice, tc.in); r.Code != http.StatusBadRequest || !strings.Contains(r.Body.String(), tc.want) {
			t.Errorf("%+v: %d %s", tc.in, r.Code, r.Body)
		}
	}
	// In-cluster services may use plain HTTP.
	if r := do(t, h, "POST", "/api/server-requests", alice, RequestInput{ServerName: "crm", URL: "http://crm.tools.svc:8080/mcp"}); r.Code != http.StatusCreated {
		t.Fatalf("in-cluster: %d %s", r.Code, r.Body)
	}
	r := do(t, h, "POST", "/api/server-requests", alice, RequestInput{ServerName: "jira", DisplayName: "Jira", URL: "https://mcp.atlassian.example/v1/mcp", Connection: "jira"})
	if r.Code != http.StatusCreated {
		t.Fatalf("request: %d %s", r.Code, r.Body)
	}
	req := decodeAs[RequestView](t, r.Body.String())
	if req.URL != "https://mcp.atlassian.example/v1/mcp" || req.Connection != "jira" || req.Transport != "streamable-http" || req.Image != "" {
		t.Fatalf("request view %+v", req)
	}
	if r := do(t, h, "POST", "/api/server-requests/"+req.Name+"/approve", sam, DecisionInput{}); r.Code != 200 {
		t.Fatalf("approve: %d %s", r.Code, r.Body)
	}
	var srv v1.Server
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &srv); err != nil || srv.Spec.URL != req.URL || srv.Spec.Connection != "jira" {
		t.Fatalf("server %+v %v", srv.Spec, err)
	}
	var list []ServerView
	list = decodeAs[[]ServerView](t, do(t, h, "GET", "/api/servers", alice, nil).Body.String())
	for _, s := range list {
		if s.Name == "jira" && (s.URL != req.URL || s.Connection != "jira") {
			t.Fatalf("server view %+v", s)
		}
	}
}

func TestRemoteServerURLMustParse(t *testing.T) {
	h, _, a := serversFixture(t)
	alice := user(a, "alice@acme.test", RoleViewer)
	for _, u := range []string{"https://example.com:bad/mcp", "https://example.com/%zz", "https://:443/mcp"} {
		if r := do(t, h, "POST", "/api/server-requests", alice, RequestInput{ServerName: "x", URL: u}); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", u, r.Code, r.Body)
		}
	}
}

// My access matches rules that name the signed-in user's email (canonical form), not only their groups.
func TestMyAccessByEmail(t *testing.T) {
	h, c, a := serversFixture(t)
	if err := c.Create(t.Context(), &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "pilot", Namespace: testNS},
		Spec: v1.AccessRuleSpec{Users: []string{"mira@acme.test"}, Servers: []v1.ServerAccess{{Name: "time"}}}}); err != nil {
		t.Fatal(err)
	}
	got := decodeAs[MyAccess](t, do(t, h, "GET", "/api/me/access", user(a, "Mira@Acme.test", RoleViewer, "ml-research"), nil).Body.String())
	if len(got.Servers) != 1 || got.Servers[0].Server != "time" || !got.Servers[0].AllTools {
		t.Fatalf("mira: %+v", got)
	}
	other := decodeAs[MyAccess](t, do(t, h, "GET", "/api/me/access", user(a, "bob@acme.test", RoleViewer, "ml-research"), nil).Body.String())
	if len(other.Servers) != 0 {
		t.Fatalf("bob: %+v", other)
	}
}
