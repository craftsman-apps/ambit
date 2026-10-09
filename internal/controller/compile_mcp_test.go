package controller

import (
	"strconv"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const timeImage = "ghcr.io/stacklok/dockyard/uvx/mcp-server-time@sha256:0000000000000000000000000000000000000000000000000000000000000001"

func server(name, transport string) v1.Server {
	return v1.Server{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system", UID: types.UID("uid-" + name),
		Annotations: map[string]string{"argocd.argoproj.io/tracking-id": "ambit-config:ambitproject.dev/Server:ambit-system/" + name}},
		Spec: v1.ServerSpec{Image: timeImage, Transport: transport},
		// Running in ToolHive: only running image servers are targets of the shared backend.
		Status: v1.ServerStatus{Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Running"},
			{Type: "Serving", Status: metav1.ConditionTrue, Reason: "Answered"}}}}
}

func toolRule(name string, groups []string, all bool, servers ...v1.ServerAccess) v1.AccessRule {
	return v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system"},
		Spec: v1.AccessRuleSpec{Groups: groups, AllSignedIn: all, Servers: servers}}
}

func find(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no %s %s in output", kind, name)
	return nil
}

func mcpExpr(t *testing.T, r Result) string {
	t.Helper()
	p := find(t, r.Objects, "AgentgatewayPolicy", MCPBackendName+"-access")
	e, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "backend", "mcp", "authorization", "policy", "matchExpressions")
	if len(e) != 1 {
		t.Fatalf("want exactly one match expression, got %v", e)
	}
	return e[0]
}

func TestNoServersGeneratesNoMCPObjects(t *testing.T) {
	r := Compile([]v1.Model{model("gemma", "google/gemma:free")}, nil, nil, route)
	for _, o := range r.Objects {
		if o.GetKind() != "AgentgatewayModel" {
			t.Fatalf("unexpected %s %s", o.GetKind(), o.GetName())
		}
	}
}

func TestServerWithoutRulesIsDeniedToEveryone(t *testing.T) {
	r := Compile(nil, []v1.Server{server("time", "stdio")}, nil, route)
	if got := mcpExpr(t, r); got != "false" {
		t.Fatalf("got %q, want deny-all", got)
	}
}

func TestToolRulesCompileToOneExpression(t *testing.T) {
	r := Compile(nil, []v1.Server{server("time", "stdio"), server("everything", "stdio")}, []v1.AccessRule{
		toolRule("eng-time", []string{"eng"}, false, v1.ServerAccess{Name: "time", Tools: []string{"get_current_time"}}),
		toolRule("ml-time", []string{"ml-research"}, false, v1.ServerAccess{Name: "time", Tools: []string{"get_current_time", "convert_time"}}),
		toolRule("ml-all", []string{"ml-research"}, false, v1.ServerAccess{Name: "everything"}),
	}, route)
	want := `mcp.tool.target == "everything" && jwt.groups.exists(g, g in ["ml-research"])` +
		` || mcp.tool.target == "time" && mcp.tool.name == "convert_time" && jwt.groups.exists(g, g in ["ml-research"])` +
		` || mcp.tool.target == "time" && mcp.tool.name == "get_current_time" && jwt.groups.exists(g, g in ["eng", "ml-research"])`
	if got := mcpExpr(t, r); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestAllSignedInOnAllToolsDropsTheGroupCheck(t *testing.T) {
	r := Compile(nil, []v1.Server{server("time", "stdio")},
		[]v1.AccessRule{toolRule("everyone", nil, true, v1.ServerAccess{Name: "time"})}, route)
	if got := mcpExpr(t, r); got != `mcp.tool.target == "time"` {
		t.Fatalf("got %s", got)
	}
}

func TestUnknownServerMakesRuleDegraded(t *testing.T) {
	r := Compile([]v1.Model{model("gemma", "google/gemma:free")}, []v1.Server{server("time", "stdio")},
		[]v1.AccessRule{{ObjectMeta: metav1.ObjectMeta{Name: "typo"}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"},
			Models: []string{"gemmaa"}, Servers: []v1.ServerAccess{{Name: "time"}, {Name: "tiem"}}}}}, route)
	c := r.RuleConditions["typo"]
	if c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "gemmaa") || !strings.Contains(c.Message, "tiem") {
		t.Fatalf("condition: %+v", c)
	}
	if got := mcpExpr(t, r); !strings.Contains(got, `"time"`) || strings.Contains(got, "tiem") {
		t.Fatalf("valid part should still apply: %s", got)
	}
}

func TestGeneratedMCPObjects(t *testing.T) {
	r := Compile(nil, []v1.Server{server("time", "stdio"), server("legacy", "sse")}, nil, route)
	ms := find(t, r.Objects, "MCPServer", "time")
	if ms.GetAPIVersion() != "toolhive.stacklok.dev/v1beta1" {
		t.Fatalf("apiVersion %s", ms.GetAPIVersion())
	}
	if img, _, _ := unstructured.NestedString(ms.Object, "spec", "image"); img != timeImage {
		t.Fatalf("image %s", img)
	}
	if refs := ms.GetOwnerReferences(); len(refs) != 1 || refs[0].Kind != "Server" || refs[0].UID != "uid-time" || !*refs[0].Controller {
		t.Fatalf("ownerReferences %+v", refs)
	}
	for _, o := range r.Objects {
		for k := range o.GetAnnotations() {
			if strings.HasPrefix(k, "argocd.argoproj.io/") {
				t.Fatalf("%s %s copied %s", o.GetKind(), o.GetName(), k)
			}
		}
	}

	b := find(t, r.Objects, "AgentgatewayBackend", MCPBackendName)
	if fm, _, _ := unstructured.NestedString(b.Object, "spec", "mcp", "failureMode"); fm != "FailOpen" {
		t.Fatalf("failureMode %s", fm)
	}
	targets, _, _ := unstructured.NestedSlice(b.Object, "spec", "mcp", "targets")
	if len(targets) != 2 {
		t.Fatalf("targets %v", targets)
	}
	legacy, timeT := targets[0].(map[string]any), targets[1].(map[string]any) // sorted by name
	if legacy["name"] != "legacy" || legacy["static"].(map[string]any)["protocol"] != "SSE" || legacy["static"].(map[string]any)["path"] != "/sse" {
		t.Fatalf("sse target %v", legacy)
	}
	st := timeT["static"].(map[string]any)
	// A DNS host, not a Service backendRef: agentgateway rejects the whole shared backend while any referenced
	// Service is missing, which ToolHive's proxy Service is around every create and delete (risk 16).
	if st["protocol"] != "StreamableHTTP" || st["path"] != "/mcp" || st["port"] != int64(8080) ||
		st["host"] != "mcp-time-proxy.ambit-system.svc" || st["backendRef"] != nil {
		t.Fatalf("stdio target %v", timeT)
	}
	// Shared objects belong to every Server, so they are removed with the last one and never before.
	for _, o := range []*unstructured.Unstructured{b, find(t, r.Objects, "AgentgatewayPolicy", MCPBackendName+"-access")} {
		refs := o.GetOwnerReferences()
		if len(refs) != 2 || refs[0].Name != "legacy" || refs[1].Name != "time" || (refs[0].Controller != nil && *refs[0].Controller) {
			t.Fatalf("%s ownerReferences %+v", o.GetKind(), refs)
		}
	}
}

func TestServerCELIsThePartForOneServer(t *testing.T) {
	servers := []v1.Server{server("time", "stdio"), server("everything", "stdio")}
	rules := []v1.AccessRule{
		toolRule("eng-time", []string{"eng"}, false, v1.ServerAccess{Name: "time", Tools: []string{"get_current_time"}}),
		toolRule("ml-all", []string{"ml-research"}, false, v1.ServerAccess{Name: "everything"}),
	}
	if got := ServerCEL(servers, rules, "time"); got != `mcp.tool.target == "time" && mcp.tool.name == "get_current_time" && jwt.groups.exists(g, g in ["eng"])` {
		t.Fatalf("time: %s", got)
	}
	if got := ServerCEL(servers, nil, "time"); got != "false" {
		t.Fatalf("no rules: %s", got)
	}
	if got := ServerCEL(servers, rules, "missing"); got != "false" {
		t.Fatalf("unknown server: %s", got)
	}
}

func TestRemoteServersAndPerUserTokens(t *testing.T) {
	gh := v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "ambit-system", UID: "uid-github"},
		Spec: v1.ServerSpec{URL: "https://api.githubcopilot.com/mcp/", Connection: "github"}}
	who := v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "whoami", Namespace: "ambit-system", UID: "uid-whoami"},
		Spec: v1.ServerSpec{URL: "http://whoami.ambit-test.svc:8080/mcp"}}
	r := Compile(nil, []v1.Server{gh, who}, nil, route)
	for _, o := range r.Objects {
		if o.GetKind() == "MCPServer" {
			t.Fatalf("remote servers need no ToolHive MCPServer, got %s", o.GetName())
		}
	}
	targets, _, _ := unstructured.NestedSlice(find(t, r.Objects, "AgentgatewayBackend", MCPBackendName).Object, "spec", "mcp", "targets")
	ghT, whoT := targets[0].(map[string]any)["static"].(map[string]any), targets[1].(map[string]any)["static"].(map[string]any)
	if ghT["host"] != "api.githubcopilot.com" || ghT["port"] != int64(443) || ghT["path"] != "/mcp/" || ghT["policies"] == nil {
		t.Fatalf("https target %v", ghT)
	}
	if whoT["host"] != "whoami.ambit-test.svc" || whoT["port"] != int64(8080) || whoT["path"] != "/mcp" || whoT["policies"] != nil {
		t.Fatalf("http target %v", whoT)
	}
	p := find(t, r.Objects, "AgentgatewayPolicy", "ambit-conn-github")
	refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	if refs[0].(map[string]any)["sectionName"] != "github" {
		t.Fatalf("policy must target only the github server: %v", refs)
	}
	ea, _, _ := unstructured.NestedMap(p.Object, "spec", "backend", "extAuth")
	h := ea["http"].(map[string]any)
	if h["path"] != `"/v1/connection/github"` || h["addRequestHeaders"].(map[string]any)["x-ambit-user"] != "jwt.email" ||
		ea["failureMode"] != "FailOpen" || ea["backendRef"].(map[string]any)["port"] != int64(BrokerPort) {
		t.Fatalf("extAuth %v", ea)
	}
	for _, o := range r.Objects {
		if o.GetName() == "ambit-conn-whoami" {
			t.Fatal("a server without a connection gets no token policy")
		}
	}
}

// Review finding (Codex): a URL that passes the CRD pattern but does not parse ("host:bad") must not
// crash the controller; the server is left out of the gateway's targets.
func TestMalformedRemoteURLIsSkipped(t *testing.T) {
	bad := server("bad", "streamable-http")
	bad.Spec.Image, bad.Spec.URL = "", "https://example.com:bad/mcp"
	good := server("time", "stdio")
	r := Compile(nil, []v1.Server{bad, good}, nil, route)
	b := find(t, r.Objects, "AgentgatewayBackend", MCPBackendName)
	targets, _, _ := unstructured.NestedSlice(b.Object, "spec", "mcp", "targets")
	if len(targets) != 1 || targets[0].(map[string]any)["name"] != "time" {
		t.Fatalf("targets %v", targets)
	}
}

// Argument limits are enforced by ambit-guard. Without it (protection off), a grant with limits must not
// become an unlimited grant: it grants nothing, and the rule says why.
func TestArgumentLimitsWithoutTheGuardGrantNothing(t *testing.T) {
	limited := toolRule("eng-limited", []string{"eng"}, false, v1.ServerAccess{Name: "time", Tools: []string{"convert_time"},
		Arguments: []v1.ArgumentRule{{Name: "time", OneOf: []string{"12:00"}}}})
	plain := toolRule("eng-plain", []string{"eng"}, false, v1.ServerAccess{Name: "time", Tools: []string{"get_current_time"}})
	servers := []v1.Server{server("time", "stdio")}
	rules, dropped := WithoutArgumentLimits([]v1.AccessRule{limited, plain})
	r := Compile(nil, servers, rules, route)
	ApplyDropped(&r, dropped)
	e := mcpExpr(t, r)
	if strings.Contains(e, "convert_time") || !strings.Contains(e, "get_current_time") {
		t.Fatalf("limited grant leaked or plain grant lost: %s", e)
	}
	c := r.RuleConditions["eng-limited"]
	if c.Status != "False" || c.Reason != "ArgumentLimitsNeedProtection" || !strings.Contains(c.Message, "time") {
		t.Fatalf("condition %+v", c)
	}
	if r.RuleConditions["eng-plain"].Status != "True" {
		t.Fatalf("plain rule %+v", r.RuleConditions["eng-plain"])
	}
	// With the guard, the limited grant compiles like any grant (the guard checks the arguments).
	r = Compile(nil, servers, []v1.AccessRule{limited, plain}, route)
	if e := mcpExpr(t, r); !strings.Contains(e, "convert_time") {
		t.Fatalf("with the guard: %s", e)
	}
}

func TestBoundConnectionGetsDestinationPath(t *testing.T) {
	servers := []v1.Server{perUser("github", "https://api.githubcopilot.com/mcp/", "gh"), perUser("legacy", "https://x.test/mcp", "old")}
	conns := []v1.Connection{
		{ObjectMeta: metav1.ObjectMeta{Name: "gh"}, Spec: v1.ConnectionSpec{ServerURL: "https://api.githubcopilot.com/mcp/"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "old"}},
	}
	res := Compile(nil, servers, nil, RouteRef{Name: "ambit-llm"})
	SetBrokerPaths(res.Objects, servers, conns)
	paths := map[string]string{}
	for _, o := range res.Objects {
		if p, ok, _ := unstructured.NestedString(o.Object, "spec", "backend", "extAuth", "http", "path"); ok {
			paths[o.GetName()] = p
		}
	}
	want := strconv.Quote("/v1/connection/gh/server/github/" + DestinationHash("https://api.githubcopilot.com/mcp/"))
	if paths["ambit-conn-github"] != want || paths["ambit-conn-legacy"] != `"/v1/connection/old"` {
		t.Fatalf("paths %v", paths)
	}
}

func TestServerAccessSummary(t *testing.T) {
	servers := []v1.Server{perUser("crm", "https://mcp.crm.test/mcp", "crm")}
	rules := []v1.AccessRule{
		{Spec: v1.AccessRuleSpec{AllSignedIn: true, Servers: []v1.ServerAccess{{Name: "crm", Tools: []string{"get_record"}}}}},
		{Spec: v1.AccessRuleSpec{Groups: []string{"sales"}, Servers: []v1.ServerAccess{{Name: "crm"}}}},
	}
	a := ServerAccess(servers, nil, rules, "crm")
	if !a.AllTools || !a.AllSignedIn || len(a.Groups) != 1 || a.Groups[0] != "sales" || len(a.Tools["get_record"]) != 0 {
		t.Fatalf("%+v", a)
	}
	if _, ok := a.Tools["get_record"]; !ok {
		t.Fatalf("tool missing: %+v", a)
	}
	if b := ServerAccess(servers, nil, rules, "nope"); b.AllTools || b.AllSignedIn || len(b.Tools) != 0 {
		t.Fatalf("unknown server: %+v", b)
	}
}

func TestServerAccessEmptyWhenReconcilerLeavesServerOut(t *testing.T) {
	rules := []v1.AccessRule{{Spec: v1.AccessRuleSpec{AllSignedIn: true, Servers: []v1.ServerAccess{{Name: "crm"}}}}}
	conns := []v1.Connection{{ObjectMeta: metav1.ObjectMeta{Name: "crm"}, Spec: v1.ConnectionSpec{ServerURL: "https://other.test/mcp"}}}
	refused := []v1.Server{perUser("crm", "https://mcp.crm.test/mcp", "crm")}
	if a := ServerAccess(refused, conns, rules, "crm"); a.AllTools || a.AllSignedIn {
		t.Fatalf("bound elsewhere: %+v", a)
	}
	bad := []v1.Server{perUser("crm", "https://mcp.crm.test:bad/mcp", "crm")}
	if a := ServerAccess(bad, nil, rules, "crm"); a.AllTools || a.AllSignedIn {
		t.Fatalf("bad url: %+v", a)
	}
}

func backendTargets(t *testing.T, objs []*unstructured.Unstructured) []string {
	t.Helper()
	targets, _, _ := unstructured.NestedSlice(find(t, objs, "AgentgatewayBackend", MCPBackendName).Object, "spec", "mcp", "targets")
	var names []string
	for _, x := range targets {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	return names
}

// Only running image servers are targets: agentgateway opens each session by contacting every target, so a
// server that is still starting (or still stopping, when recreated at once) slowed every user's new session
// for 1-5 s (risk 16, measured on kind). Its MCPServer is still generated, so ToolHive starts it.
func TestOnlyRunningImageServersAreTargets(t *testing.T) {
	starting := server("everything", "stdio")
	starting.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionUnknown, Reason: "Pending"}}
	remote := server("remote", "stdio")
	remote.Spec.Image, remote.Spec.URL, remote.Status.Conditions = "", "https://mcp.example.com/mcp", nil
	r := Compile(nil, []v1.Server{server("time", "stdio"), starting, remote}, nil, route)
	find(t, r.Objects, "MCPServer", "everything")
	if got := strings.Join(backendTargets(t, r.Objects), ","); got != "remote,time" {
		t.Fatalf("targets %s, want remote,time (a URL server needs no ToolHive pod)", got)
	}
}

// agentgateway refuses an MCP backend without targets: while no server runs yet there is no shared backend. The
// access policy stays: were it deleted too, the backend could come back before its policy, open to every user.
func TestNoRunningServerGeneratesNoSharedBackend(t *testing.T) {
	starting := server("time", "stdio")
	starting.Status.Conditions = nil
	r := Compile(nil, []v1.Server{starting}, nil, route)
	find(t, r.Objects, "MCPServer", "time")
	find(t, r.Objects, "AgentgatewayPolicy", MCPBackendName+"-access")
	for _, o := range r.Objects {
		if o.GetKind() == "AgentgatewayBackend" {
			t.Fatalf("generated %s %s with no running server", o.GetKind(), o.GetName())
		}
	}
}

// A down or unreachable server fails within 1 s instead of ~10 s when a session opens (agentgateway contacts every
// target first). Only connection setup is limited: long tool calls and streams are not.
func TestAccessPolicyLimitsConnectTime(t *testing.T) {
	r := Compile(nil, []v1.Server{server("time", "stdio")}, nil, route)
	p := find(t, r.Objects, "AgentgatewayPolicy", MCPBackendName+"-access")
	if got, _, _ := unstructured.NestedString(p.Object, "spec", "backend", "tcp", "connectTimeout"); got != "1s" {
		t.Fatalf("connectTimeout %q", got)
	}
}
