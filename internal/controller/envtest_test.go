package controller

// Integration tests against a real API server and etcd (controller-runtime envtest), without a
// cluster: the CRD validation rules, and the reconciler with agentgateway and ToolHive played by
// the test. Run with `make test-go`, which downloads the binaries; plain `go test` skips them.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	envClient client.Client
	envCfg    *envtest.Environment
	envScheme = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("envtest: KUBEBUILDER_ASSETS not set, integration tests skipped (run make test-go)")
		os.Exit(m.Run())
	}
	_ = clientgoscheme.AddToScheme(envScheme)
	_ = v1.AddToScheme(envScheme)
	envCfg = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "charts", "ambit-crds", "templates", "ambit"), filepath.Join("..", "..", "deploy", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := envCfg.Start()
	if err != nil {
		fmt.Println("envtest start:", err)
		os.Exit(1)
	}
	envCfg.Config = cfg
	envClient, err = client.New(cfg, client.Options{Scheme: envScheme})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	code := m.Run()
	_ = envCfg.Stop()
	os.Exit(code)
}

func needEnv(t *testing.T) {
	t.Helper()
	if envClient == nil {
		t.Skip("KUBEBUILDER_ASSETS not set")
	}
}

func namespace(t *testing.T, name string) {
	t.Helper()
	if err := envClient.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatal(err)
	}
}

const digestImage = "ghcr.io/stacklok/dockyard/uvx/mcp-server-time@sha256:ab8255cf7bf2bdad14086b356790d2d61d06cab221b85950805427be9d06d949"

// The API server enforces these rules, so a kubectl or Git user meets them too, not only the console.
func TestCRDRulesRejectUnsafeObjects(t *testing.T) {
	needEnv(t)
	ns := "crd-rules"
	namespace(t, ns)
	ctx := t.Context()
	reject := func(what string, o client.Object, want string) {
		t.Helper()
		err := envClient.Create(ctx, o)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", what, want, err)
		}
	}
	reject("image by tag", &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "tagged", Namespace: ns},
		Spec: v1.ServerSpec{Image: "ghcr.io/stacklok/dockyard/uvx/mcp-server-time:2026.8.18"}}, "sha256")
	reject("name over 40 characters", &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 41), Namespace: ns},
		Spec: v1.ServerSpec{Image: digestImage}}, "at most 40 characters")
	reject("rule with no model and no server", &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns},
		Spec: v1.AccessRuleSpec{Groups: []string{"eng"}}}, "at least one model or server")
	reject("tool name that could break CEL", &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "inject", Namespace: ns},
		Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Servers: []v1.ServerAccess{{Name: "time", Tools: []string{`x" || true || "`}}}}}, "spec.servers[0].tools[0]")

	// "tools: []" must be refused, not read as "all tools". Sent as raw JSON, since omitempty would drop it.
	raw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1.GroupVersion.String(), "kind": "AccessRule",
		"metadata": map[string]any{"name": "no-tools", "namespace": ns},
		"spec":     map[string]any{"groups": []any{"eng"}, "servers": []any{map[string]any{"name": "time", "tools": []any{}}}},
	}}
	reject("empty tool list", raw, "should have at least 1 items")

	q := &v1.ServerRequest{ObjectMeta: metav1.ObjectMeta{Name: "time-req", Namespace: ns},
		Spec: v1.ServerRequestSpec{ServerName: "time", RequestedBy: "alice@acme.test", Server: v1.ServerSpec{Image: digestImage, Transport: "stdio"}}}
	if err := envClient.Create(ctx, q); err != nil {
		t.Fatal(err)
	}
	q.Spec.Server.Image = strings.Replace(digestImage, "ab82", "0000", 1)
	if err := envClient.Update(ctx, q); err == nil || !strings.Contains(err.Error(), "cannot be changed") {
		t.Errorf("changing an approved-to-be image: %v", err)
	}
	if err := envClient.Get(ctx, client.ObjectKeyFromObject(q), q); err != nil {
		t.Fatal(err)
	}
	q.Status = v1.ServerRequestStatus{Decision: "Approved", DecidedBy: "sam@acme.test"}
	if err := envClient.Status().Update(ctx, q); err != nil {
		t.Fatal(err)
	}
	q.Status.Decision = "Denied"
	if err := envClient.Status().Update(ctx, q); err == nil || !strings.Contains(err.Error(), "decision cannot be changed") {
		t.Errorf("changing a decision: %v", err)
	}
}

// eventually polls f every 100 ms for up to 15 s.
func eventually(t *testing.T, what string, f func() error) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if err = f(); err == nil {
			return
		}
	}
	t.Fatalf("%s: %v", what, err)
}

func readyOf(conds []metav1.Condition) string {
	if c := meta.FindStatusCondition(conds, "Ready"); c != nil {
		return c.Reason
	}
	return "none"
}

func TestReconcilerAgainstAPIServer(t *testing.T) {
	needEnv(t)
	ns := "ambit-it"
	namespace(t, ns)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr, err := ctrl.NewManager(envCfg.Config, ctrl.Options{Scheme: envScheme,
		Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
		Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	tools := []v1.Tool{{Name: "convert_time"}, {Name: "get_current_time", Description: "Current time"}}
	r := &Reconciler{Client: mgr.GetClient(), Live: mgr.GetAPIReader(), Namespace: ns, Route: RouteRef{Name: "ambit-llm"},
		ListTools: func(context.Context, v1.Server) ([]v1.Tool, error) { return tools, nil }}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	for _, o := range []client.Object{
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns},
			Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma-4-31b-it:free", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-openrouter"}}},
		&v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "time", Namespace: ns}, Spec: v1.ServerSpec{Image: digestImage}},
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"},
			Models: []string{"gemma"}, Servers: []v1.ServerAccess{{Name: "time", Tools: []string{"get_current_time"}}}}},
	} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	get := func(gvkKind, name string) (*unstructured.Unstructured, error) {
		u := &unstructured.Unstructured{}
		for _, g := range generatedGVKs {
			if g.Kind == gvkKind {
				u.SetGroupVersionKind(g)
			}
		}
		return u, envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, u)
	}
	// Play ToolHive: the server runs, so it joins the shared backend (only running image servers do).
	eventually(t, "MCPServer", func() error { _, err := get("MCPServer", "time"); return err })
	setMCPServerReady(t, ns, "time", true)
	eventually(t, "generated objects", func() error {
		for _, k := range [][2]string{{"AgentgatewayModel", "gemma"}, {"MCPServer", "time"}, {"AgentgatewayBackend", MCPBackendName}, {"AgentgatewayPolicy", MCPBackendName + "-access"}} {
			if _, err := get(k[0], k[1]); err != nil {
				return err
			}
		}
		return nil
	})
	// The policy can exist before the AccessRule is compiled into it (it then grants nothing: "false").
	eventually(t, "MCP access policy", func() error {
		p, err := get("AgentgatewayPolicy", MCPBackendName+"-access")
		if err != nil {
			return err
		}
		if e, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "backend", "mcp", "authorization", "policy", "matchExpressions"); len(e) != 1 ||
			e[0] != `mcp.tool.target == "time" && mcp.tool.name == "get_current_time" && jwt.groups.exists(g, g in ["eng"])` {
			return fmt.Errorf("policy %v", e)
		}
		return nil
	})
	var m v1.Model
	eventually(t, "Model Pending before agentgateway answers", func() error {
		_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "gemma"}, &m)
		if readyOf(m.Status.Conditions) != "Pending" {
			return fmt.Errorf("reason %s", readyOf(m.Status.Conditions))
		}
		return nil
	})

	// Play agentgateway: accept the generated model. The Model must follow (the 4a stuck-Pending bug).
	am, _ := get("AgentgatewayModel", "gemma")
	_ = unstructured.SetNestedSlice(am.Object, []any{map[string]any{
		"parentRef":      map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "ambit-llm", "namespace": ns},
		"controllerName": "agentgateway.dev/agentgateway",
		"conditions": []any{map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted", "message": "ok",
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339), "observedGeneration": am.GetGeneration()}},
	}}, "status", "parents")
	if err := envClient.Status().Update(ctx, am); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Model Ready after agentgateway accepts", func() error {
		_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "gemma"}, &m)
		if readyOf(m.Status.Conditions) != "Accepted" {
			return fmt.Errorf("reason %s", readyOf(m.Status.Conditions))
		}
		return nil
	})

	// Play ToolHive: report the server running. The Server becomes Ready and lists its tools.
	ms, _ := get("MCPServer", "time")
	_ = unstructured.SetNestedField(ms.Object, "Ready", "status", "phase")
	_ = unstructured.SetNestedSlice(ms.Object, []any{map[string]any{"type": "Ready", "status": "True", "reason": "Ready", "message": "ok",
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}}, "status", "conditions")
	if err := envClient.Status().Update(ctx, ms); err != nil {
		t.Fatal(err)
	}
	var s v1.Server
	eventually(t, "Server Running with tools", func() error {
		_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "time"}, &s)
		if readyOf(s.Status.Conditions) != "Running" || len(s.Status.Tools) != 2 || s.Status.Tools[1].Description != "Current time" {
			return fmt.Errorf("reason %s, tools %v", readyOf(s.Status.Conditions), s.Status.Tools)
		}
		return nil
	})

	// A remote server has no ToolHive object; it is Ready as soon as it is compiled.
	remote := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "whoami", Namespace: ns}, Spec: v1.ServerSpec{URL: "http://whoami.test.svc:8080/mcp", Connection: "saas"}}
	if err := envClient.Create(ctx, remote); err != nil {
		t.Fatal(err)
	}
	eventually(t, "remote Server Ready", func() error {
		_ = envClient.Get(ctx, client.ObjectKeyFromObject(remote), remote)
		if readyOf(remote.Status.Conditions) != "Remote" {
			return fmt.Errorf("reason %s", readyOf(remote.Status.Conditions))
		}
		return nil
	})
	eventually(t, "per-user token policy", func() error { _, err := get("AgentgatewayPolicy", "ambit-conn-whoami"); return err })

	// Drift: a manual edit that opens every tool is put back.
	p, _ := get("AgentgatewayPolicy", MCPBackendName+"-access")
	_ = unstructured.SetNestedStringSlice(p.Object, []string{"true"}, "spec", "backend", "mcp", "authorization", "policy", "matchExpressions")
	if err := envClient.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, "manual edit reverted", func() error {
		p, _ := get("AgentgatewayPolicy", MCPBackendName+"-access")
		e, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "backend", "mcp", "authorization", "policy", "matchExpressions")
		if len(e) != 1 || e[0] == "true" {
			return fmt.Errorf("still %v", e)
		}
		return nil
	})
}

func genGet(ctx context.Context, ns, kind, name string) (*unstructured.Unstructured, error) {
	var u unstructured.Unstructured
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "agentgateway.dev", Version: "v1alpha1", Kind: kind})
	err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &u)
	return &u, err
}

func boundConn(ns string) *v1.Connection {
	return &v1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: ns}, Spec: v1.ConnectionSpec{DisplayName: "GitHub",
		AuthorizationURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token",
		ClientID: "x", ClientSecretRef: v1.SecretRef{Name: "ambit-conn-gh"}, ServerURL: "https://api.githubcopilot.com/mcp/"}}
}

func TestServerURLChangedAfterBindingIsRemoved(t *testing.T) {
	needEnv(t)
	ns := "ambit-bind"
	namespace(t, ns)
	ctx := t.Context()
	s := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: ns}, Spec: v1.ServerSpec{URL: "https://api.githubcopilot.com/mcp/", Connection: "gh", Transport: "streamable-http"}}
	for _, o := range []client.Object{boundConn(ns), s} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := genGet(ctx, ns, "AgentgatewayPolicy", "ambit-conn-github"); err != nil {
		t.Fatalf("bound server not compiled: %v", err)
	}
	if _, err := genGet(ctx, ns, "AgentgatewayBackend", MCPBackendName); err != nil {
		t.Fatalf("bound server not compiled: %v", err)
	}
	if err := envClient.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil { // status was written
		t.Fatal(err)
	}
	s.Spec.URL = "https://evil.test/mcp"
	if err := envClient.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	// The only Server is refused: its connection policy and the shared backend and access policy are gone.
	for _, o := range [][2]string{{"AgentgatewayPolicy", "ambit-conn-github"}, {"AgentgatewayBackend", MCPBackendName}, {"AgentgatewayPolicy", MCPBackendName + "-access"}} {
		if _, err := genGet(ctx, ns, o[0], o[1]); !apierrors.IsNotFound(err) {
			t.Fatalf("%s/%s still present: %v", o[0], o[1], err)
		}
	}
	_ = envClient.Get(ctx, client.ObjectKeyFromObject(s), s)
	if readyOf(s.Status.Conditions) != "ConnectionBound" {
		t.Fatalf("reason %s", readyOf(s.Status.Conditions))
	}
	// Back to the bound URL: the policy and backend are compiled again.
	s.Spec.URL = "https://api.githubcopilot.com/mcp/"
	if err := envClient.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	for _, o := range [][2]string{{"AgentgatewayPolicy", "ambit-conn-github"}, {"AgentgatewayBackend", MCPBackendName}} {
		if _, err := genGet(ctx, ns, o[0], o[1]); err != nil {
			t.Fatalf("%s/%s not restored: %v", o[0], o[1], err)
		}
	}
}

func TestRefusedServerLeavesKeptServerInBackend(t *testing.T) {
	needEnv(t)
	ns := "ambit-bind2"
	namespace(t, ns)
	ctx := t.Context()
	good := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: ns}, Spec: v1.ServerSpec{URL: "https://api.githubcopilot.com/mcp/", Connection: "gh", Transport: "streamable-http"}}
	evil := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "evil", Namespace: ns}, Spec: v1.ServerSpec{URL: "https://evil.test/mcp", Connection: "gh", Transport: "streamable-http"}}
	for _, o := range []client.Object{boundConn(ns), good, evil} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	b, err := genGet(ctx, ns, "AgentgatewayBackend", MCPBackendName)
	if err != nil {
		t.Fatal(err)
	}
	targets, _, _ := unstructured.NestedSlice(b.Object, "spec", "mcp", "targets")
	if len(targets) != 1 || !strings.Contains(fmt.Sprint(targets[0]), "github") || strings.Contains(fmt.Sprint(targets), "evil") {
		t.Fatalf("targets %v", targets)
	}
	if _, err := genGet(ctx, ns, "AgentgatewayPolicy", "ambit-conn-evil"); !apierrors.IsNotFound(err) {
		t.Fatalf("refused server's policy present: %v", err)
	}
	_ = envClient.Get(ctx, client.ObjectKeyFromObject(evil), evil)
	if readyOf(evil.Status.Conditions) != "ConnectionBound" {
		t.Fatalf("reason %s", readyOf(evil.Status.Conditions))
	}
}

// deleteFails fails every delete of one generated object name.
type deleteFails struct {
	client.Client
	prefix string
}

func (d deleteFails) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if strings.HasPrefix(obj.GetName(), d.prefix) {
		return fmt.Errorf("injected delete failure")
	}
	return d.Client.Delete(ctx, obj, opts...)
}

func TestFailedCleanupOfRefusedServerIsRetried(t *testing.T) {
	needEnv(t)
	ns := "ambit-bind3"
	namespace(t, ns)
	ctx := t.Context()
	s := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: ns}, Spec: v1.ServerSpec{URL: "https://api.githubcopilot.com/mcp/", Connection: "gh", Transport: "streamable-http"}}
	for _, o := range []client.Object{boundConn(ns), s} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if err := envClient.Get(ctx, client.ObjectKeyFromObject(s), s); err != nil {
		t.Fatal(err)
	}
	s.Spec.URL = "https://evil.test/mcp"
	if err := envClient.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	r.Client = deleteFails{Client: envClient, prefix: "ambit-conn-"}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err == nil {
		t.Fatal("a failed delete of a refused server's policy was not returned")
	}
}

// The CRD's rules hold in the real API server, not only in `ambit validate`.
func TestAPIServerRefusesVertexAIWithoutSettings(t *testing.T) {
	needEnv(t)
	ns := "ambit-vertex-api"
	namespace(t, ns)
	err := envClient.Create(t.Context(), &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemini", Namespace: ns},
		Spec: v1.ModelSpec{Provider: "VertexAI", Model: "gemini-2.5-pro"}})
	if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "vertexai settings are required") {
		t.Fatalf("want an Invalid error naming the vertexai rule, got %v", err)
	}
	if err := envClient.Create(t.Context(), &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemini-ok", Namespace: ns},
		Spec: v1.ModelSpec{Provider: "VertexAI", Model: "gemini-2.5-pro", VertexAI: &v1.VertexAISettings{ProjectID: "acme-ml"}}}); err != nil {
		t.Fatalf("valid VertexAI Model refused: %v", err)
	}
}

// setMCPServerReady plays ToolHive: it reports the generated MCPServer's readiness.
func setMCPServerReady(t *testing.T, ns, name string, ready bool) {
	t.Helper()
	ms := &unstructured.Unstructured{}
	ms.SetGroupVersionKind(MCPServerGVK)
	if err := envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, ms); err != nil {
		t.Fatal(err)
	}
	status, reason := "True", "Running"
	if !ready {
		status, reason = "False", "NotRunning"
	}
	_ = unstructured.SetNestedSlice(ms.Object, []any{map[string]any{"type": "Ready", "status": status, "reason": reason,
		"message": "", "lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339)}}, "status", "conditions")
	if err := envClient.Status().Update(t.Context(), ms); err != nil {
		t.Fatal(err)
	}
}

// An image server joins the shared backend once ToolHive runs it and it has answered the controller's MCP
// introduction (a booting server made every user's new session wait ~10 s), and stays listed through restarts:
// any change to the target list breaks resuming open sessions on the other gateway replica.
func TestImageServerJoinsBackendOnceItAnswers(t *testing.T) {
	needEnv(t)
	ns := "ambit-admission"
	namespace(t, ns)
	ctx := t.Context()
	if err := envClient.Create(ctx, &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "time", Namespace: ns},
		Spec: v1.ServerSpec{Image: timeImage, Transport: "stdio"}}); err != nil {
		t.Fatal(err)
	}
	answers := false
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"},
		ListTools: func(context.Context, v1.Server) ([]v1.Tool, error) {
			if !answers {
				return nil, errors.New("connection refused")
			}
			return []v1.Tool{{Name: "get_current_time"}}, nil
		}}
	rec := func() reconcile.Result {
		t.Helper()
		res, err := r.Reconcile(ctx, reconcile.Request{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	listed := func() string {
		t.Helper()
		b, err := genGet(ctx, ns, "AgentgatewayBackend", MCPBackendName)
		if apierrors.IsNotFound(err) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(backendTargets(t, []*unstructured.Unstructured{b}), ",")
	}
	server := func() v1.Server {
		t.Helper()
		var s v1.Server
		if err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "time"}, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	rec()
	if got := listed(); got != "" {
		t.Fatalf("listed before ToolHive runs it: %q", got)
	}
	setMCPServerReady(t, ns, "time", true)
	if res := rec(); res.RequeueAfter <= 0 || res.RequeueAfter > 5*time.Second {
		t.Fatalf("a running server that does not answer yet must be retried soon, got requeue %v", res.RequeueAfter)
	}
	if got := listed(); got != "" {
		t.Fatalf("listed before it answers: %q", got)
	}
	answers = true
	rec()
	if got := listed(); got != "time" {
		t.Fatalf("not listed once it answers: %q", got)
	}
	if c := meta.FindStatusCondition(server().Status.Conditions, "Serving"); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Serving condition not saved: %+v", c)
	}
	setMCPServerReady(t, ns, "time", false) // ToolHive restarts it
	answers = false
	rec()
	if got := listed(); got != "time" {
		t.Fatalf("a restart removed the server from the list: %q", got)
	}
	// The access policy carries the connect timeout and is never deleted while Servers exist.
	if _, err := genGet(ctx, ns, "AgentgatewayPolicy", MCPBackendName+"-access"); err != nil {
		t.Fatal(err)
	}
	// A pass with no news writes nothing.
	before := server().ResourceVersion
	rec()
	if after := server().ResourceVersion; before != after {
		t.Fatalf("an idle reconcile rewrote the Server (%s -> %s)", before, after)
	}
}

// The Server's saved status follows ToolHive even when its tools are not read (ListTools off).
func TestRunningServerStatusIsSaved(t *testing.T) {
	needEnv(t)
	ns := "ambit-running-status"
	namespace(t, ns)
	ctx := t.Context()
	if err := envClient.Create(ctx, &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "time", Namespace: ns},
		Spec: v1.ServerSpec{Image: timeImage, Transport: "stdio"}}); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	setMCPServerReady(t, ns, "time", true)
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	var s v1.Server
	_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "time"}, &s)
	if c := meta.FindStatusCondition(s.Status.Conditions, "Ready"); c == nil || c.Reason != "Running" {
		t.Fatalf("saved Ready condition %+v, want reason Running", c)
	}
}
