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
		Spec: v1.ServerSpec{Image: timeImage, Transport: transport}}
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
	if st["protocol"] != "StreamableHTTP" || st["path"] != "/mcp" || st["port"] != int64(8080) ||
		st["backendRef"].(map[string]any)["name"] != "mcp-time-proxy" {
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
