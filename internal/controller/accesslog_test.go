package controller

import (
	"regexp"
	"slices"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const testBase = `targetRefs: [{ group: gateway.networking.k8s.io, kind: Gateway, name: ambit }]
frontend:
  accessLog:
    attributes:
      add:
        - { name: jwt.email, expression: "jwt.email" }
        - { name: ambit.router, expression: "metadata.agentgateway_user_model" }
    otlp:
      protocol: GRPC
      backendRef: { name: ambit-otel-collector, port: 4317 }
`

func baseConfigMap(spec string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: AccessLogBase, Namespace: "ambit-system", UID: "base-uid"},
		Data: map[string]string{"spec.yaml": spec}}
}

func TestAccessLogPolicyAddsModeAndRulesVersion(t *testing.T) {
	base := baseConfigMap(testBase)
	p, err := accessLogPolicy(base, "audit", "3f9a1c2b7d10", []any{map[string]any{"name": "ambit.wouldRefuse", "expression": `"n/a"`}})
	if err != nil {
		t.Fatal(err)
	}
	if p.GetKind() != "AgentgatewayPolicy" || p.GetName() != "ambit-access-log" || p.GetNamespace() != "ambit-system" {
		t.Fatalf("object %s %s/%s", p.GetKind(), p.GetNamespace(), p.GetName())
	}
	if p.GetLabels()["app.kubernetes.io/managed-by"] != "ambit-controller" {
		t.Errorf("labels %v", p.GetLabels())
	}
	if refs := p.GetOwnerReferences(); len(refs) != 1 || refs[0].Kind != "ConfigMap" || refs[0].Name != AccessLogBase || refs[0].UID != "base-uid" {
		t.Errorf("owner %v", refs)
	}
	attrs, _, _ := unstructured.NestedSlice(p.Object, "spec", "frontend", "accessLog", "attributes", "add")
	var got [][2]string
	for _, a := range attrs {
		m := a.(map[string]any)
		got = append(got, [2]string{m["name"].(string), m["expression"].(string)})
	}
	want := [][2]string{{"jwt.email", "jwt.email"}, {"ambit.router", "metadata.agentgateway_user_model"},
		{"ambit.accessMode", `"audit"`}, {"ambit.rulesVersion", `"3f9a1c2b7d10"`}, {"ambit.wouldRefuse", `"n/a"`}}
	if len(got) != len(want) {
		t.Fatalf("attributes %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attribute %d = %v, want %v", i, got[i], want[i])
		}
	}
	if refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs"); len(refs) != 1 {
		t.Errorf("targetRefs %v", refs)
	}
	if b, _, _ := unstructured.NestedString(p.Object, "spec", "frontend", "accessLog", "otlp", "backendRef", "name"); b != "ambit-otel-collector" {
		t.Errorf("otlp backend %q", b)
	}
}

func TestAccessLogPolicyRefusesABrokenBase(t *testing.T) {
	for name, spec := range map[string]string{"not YAML": "frontend: [", "empty": "", "no attribute list": "frontend: {accessLog: {}}"} {
		if _, err := accessLogPolicy(baseConfigMap(spec), "enforce", "x", nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func modelWith(name, expr string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "agentgateway.dev/v1alpha1", "kind": "AgentgatewayModel",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{"provider": "OpenAI", "policies": map[string]any{"authorization": map[string]any{"action": "Allow",
			"policy": map[string]any{"matchExpressions": []any{expr}}}}}}}
}

func policyWith(name, path, expr string) *unstructured.Unstructured {
	auth := map[string]any{"action": "Allow", "policy": map[string]any{"matchExpressions": []any{expr}}}
	spec := map[string]any{"backend": map[string]any{"mcp": map[string]any{"authorization": auth}}}
	if path == "traffic" {
		spec = map[string]any{"traffic": map[string]any{"authorization": auth}}
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "agentgateway.dev/v1alpha1", "kind": "AgentgatewayPolicy",
		"metadata": map[string]any{"name": name}, "spec": spec}}
}

func TestRulesVersionFollowsTheAuthorizationExpressionsOnly(t *testing.T) {
	a, b := modelWith("gpt", `jwt.groups.exists(g, g in ["eng"])`), modelWith("gemma", "false")
	mcp := policyWith(MCPBackendName+"-access", "mcp", `mcp.tool.target == "time"`)
	v := RulesVersion([]*unstructured.Unstructured{a, b, mcp})
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(v) {
		t.Fatalf("version %q", v)
	}
	if RulesVersion([]*unstructured.Unstructured{mcp, b, a}) != v {
		t.Error("the order of the objects changed the version")
	}
	other := a.DeepCopy()
	_ = unstructured.SetNestedField(other.Object, "Anthropic", "spec", "provider")
	if RulesVersion([]*unstructured.Unstructured{other, b, mcp}) != v {
		t.Error("a change outside authorization changed the version")
	}
	off := policyWith("ambit-offboarded", "traffic", `!(jwt.email in ["a@b"])`)
	if RulesVersion([]*unstructured.Unstructured{a, b, mcp, off}) != v {
		t.Error("the offboarded list is not an AccessRule decision")
	}
	for name, objs := range map[string][]*unstructured.Unstructured{
		"model expression": {modelWith("gpt", `jwt.groups.exists(g, g in ["ops"])`), b, mcp},
		"MCP expression":   {a, b, policyWith(MCPBackendName+"-access", "mcp", "false")},
		"model removed":    {a, mcp},
	} {
		if RulesVersion(objs) == v {
			t.Errorf("%s: version unchanged", name)
		}
	}
	if RulesVersion(nil) == "" {
		t.Error("no objects: still a version")
	}
}

// Router access and router lines are AccessRule decisions too (refuse:router, other-model).
func TestRulesVersionFollowsRouterLines(t *testing.T) {
	res, _ := compileWithRouters(catalog, grants, router("auto", "Rules", "gemma", v1.RouterRoute{Model: "sonnet", Keywords: []string{"Python"}}))
	v := RulesVersion(res.Objects)
	other := slices.Clone(grants)
	other[0] = rule("auto-users", []string{"eng"}, false, "auto") // ml loses the router
	res2, _ := compileWithRouters(catalog, other, router("auto", "Rules", "gemma", v1.RouterRoute{Model: "sonnet", Keywords: []string{"Python"}}))
	if RulesVersion(res2.Objects) == v {
		t.Error("a change of who may use the router kept the rules version")
	}
}
