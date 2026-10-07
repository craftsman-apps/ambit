package controller

import (
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func model(name, id string) v1.Model {
	return v1.Model{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system", UID: types.UID("uid-" + name),
		Annotations: map[string]string{"argocd.argoproj.io/tracking-id": "ambit-config:ambitproject.dev/Model:ambit-system/" + name},
		Labels:      map[string]string{"app.kubernetes.io/instance": "ambit-config"}},
		Spec: v1.ModelSpec{Provider: "Openrouter", Model: id, CredentialsRef: v1.SecretRef{Name: "ambit-llm-openrouter"}}}
}

func rule(name string, groups []string, all bool, models ...string) v1.AccessRule {
	return v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system"},
		Spec: v1.AccessRuleSpec{Groups: groups, AllSignedIn: all, Models: models}}
}

var route = RouteRef{Name: "ambit-llm"}

// compileModels is Compile for tests that have no MCP servers.
func compileModels(m []v1.Model, r []v1.AccessRule, rt RouteRef) Result {
	return Compile(m, nil, r, rt)
}

func expr(t *testing.T, o *unstructured.Unstructured) string {
	t.Helper()
	e, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "policies", "authorization", "policy", "matchExpressions")
	if len(e) != 1 {
		t.Fatalf("want exactly one match expression, got %v", e)
	}
	return e[0]
}

func byName(objs []*unstructured.Unstructured) map[string]*unstructured.Unstructured {
	m := map[string]*unstructured.Unstructured{}
	for _, o := range objs {
		m[o.GetName()] = o
	}
	return m
}

func TestModelWithoutRulesIsDeniedToEveryone(t *testing.T) {
	r := compileModels([]v1.Model{model("nemotron", "nvidia/nemotron:free")}, nil, route)
	if got := expr(t, byName(r.Objects)["nemotron"]); got != "false" {
		t.Fatalf("got %q, want deny-all \"false\"", got)
	}
	if a := r.Access["nemotron"]; a.AllSignedIn || len(a.Groups) != 0 {
		t.Fatalf("access should be empty: %+v", a)
	}
}

func TestRulesMergePerModelSortedAndDeduplicated(t *testing.T) {
	r := compileModels(
		[]v1.Model{model("nemotron", "nvidia/nemotron:free"), model("gemma", "google/gemma:free")},
		[]v1.AccessRule{
			rule("r1", []string{"ml-research", "finance"}, false, "nemotron"),
			rule("r2", []string{"finance"}, false, "nemotron"),
			rule("r3", nil, true, "gemma"),
		}, route)
	objs := byName(r.Objects)
	if got := expr(t, objs["nemotron"]); got != `jwt.groups.exists(g, g in ["finance", "ml-research"])` {
		t.Fatalf("nemotron: %s", got)
	}
	if got := expr(t, objs["gemma"]); got != "true" {
		t.Fatalf("gemma: %s", got)
	}
}

func TestAllSignedInWinsOverGroups(t *testing.T) {
	r := compileModels([]v1.Model{model("gemma", "google/gemma:free")},
		[]v1.AccessRule{rule("a", []string{"eng"}, false, "gemma"), rule("b", nil, true, "gemma")}, route)
	if got := expr(t, r.Objects[0]); got != "true" {
		t.Fatalf("got %s", got)
	}
}

func TestUnknownModelMakesRuleDegradedButOthersStillApply(t *testing.T) {
	r := compileModels([]v1.Model{model("gemma", "google/gemma:free")},
		[]v1.AccessRule{rule("typo", []string{"eng"}, false, "gemma", "gemmaa")}, route)
	c := r.RuleConditions["typo"]
	if c.Status != metav1.ConditionFalse || c.Reason != "UnknownModel" || !strings.Contains(c.Message, "gemmaa") {
		t.Fatalf("condition: %+v", c)
	}
	if got := expr(t, r.Objects[0]); got != `jwt.groups.exists(g, g in ["eng"])` {
		t.Fatalf("valid part of the rule should still apply: %s", got)
	}
}

func TestReadyRule(t *testing.T) {
	r := compileModels([]v1.Model{model("gemma", "google/gemma:free")}, []v1.AccessRule{rule("ok", nil, true, "gemma")}, route)
	if c := r.RuleConditions["ok"]; c.Status != metav1.ConditionTrue || c.Reason != "Compiled" {
		t.Fatalf("condition: %+v", c)
	}
}

func TestGeneratedObjectShape(t *testing.T) {
	r := compileModels([]v1.Model{model("gemma", "google/gemma-4-31b-it:free")}, nil, route)
	o := r.Objects[0]
	if o.GetAPIVersion() != "agentgateway.dev/v1alpha1" || o.GetKind() != "AgentgatewayModel" || o.GetNamespace() != "ambit-system" {
		t.Fatalf("identity: %s %s %s", o.GetAPIVersion(), o.GetKind(), o.GetNamespace())
	}
	if m, _, _ := unstructured.NestedString(o.Object, "spec", "match", "model"); m != "google/gemma-4-31b-it:free" {
		t.Fatalf("match.model %q", m)
	}
	if p, _, _ := unstructured.NestedString(o.Object, "spec", "provider"); p != "Openrouter" {
		t.Fatalf("provider %q", p)
	}
	if s, _, _ := unstructured.NestedString(o.Object, "spec", "policies", "auth", "secretRef", "name"); s != "ambit-llm-openrouter" {
		t.Fatalf("secretRef %q", s)
	}
	parents, _, _ := unstructured.NestedSlice(o.Object, "spec", "parentRefs")
	if len(parents) != 1 || parents[0].(map[string]any)["name"] != "ambit-llm" || parents[0].(map[string]any)["kind"] != "HTTPRoute" {
		t.Fatalf("parentRefs %v", parents)
	}
	refs := o.GetOwnerReferences()
	if len(refs) != 1 || refs[0].Kind != "Model" || refs[0].Name != "gemma" || refs[0].UID != "uid-gemma" || refs[0].Controller == nil || !*refs[0].Controller {
		t.Fatalf("ownerReferences %+v", refs)
	}
}

// GitOps rule 1: Argo CD must never think it owns a generated object.
func TestNoArgoCDMetadataOnGeneratedObjects(t *testing.T) {
	o := compileModels([]v1.Model{model("gemma", "google/gemma:free")}, nil, route).Objects[0]
	for k := range o.GetAnnotations() {
		if strings.HasPrefix(k, "argocd.argoproj.io/") {
			t.Fatalf("copied annotation %s", k)
		}
	}
	if _, ok := o.GetLabels()["app.kubernetes.io/instance"]; ok {
		t.Fatal("copied app.kubernetes.io/instance label")
	}
	if o.GetLabels()["app.kubernetes.io/managed-by"] != "ambit" {
		t.Fatalf("labels %v", o.GetLabels())
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	models := []v1.Model{model("b", "x/b:free"), model("a", "x/a:free")}
	rules := []v1.AccessRule{rule("r2", []string{"z", "y"}, false, "a"), rule("r1", []string{"x"}, false, "a", "b")}
	first := compileModels(models, rules, route)
	second := compileModels([]v1.Model{models[1], models[0]}, []v1.AccessRule{rules[1], rules[0]}, route)
	for i := range first.Objects {
		if first.Objects[i].GetName() != second.Objects[i].GetName() || expr(t, first.Objects[i]) != expr(t, second.Objects[i]) {
			t.Fatal("input order changed the output; Argo CD and git diffs would flap")
		}
	}
}

// A self-hosted, OpenAI-compatible model is reached at its baseURL; others use the provider's endpoint.
func TestModelBaseURL(t *testing.T) {
	self := model("llama", "meta/llama-3-8b")
	self.Spec.Provider, self.Spec.BaseURL = "OpenAI", "http://vllm.models.svc:8000/v1"
	r := Compile([]v1.Model{self, model("gemma", "google/gemma:free")}, nil, nil, route)
	got, _, _ := unstructured.NestedString(find(t, r.Objects, "AgentgatewayModel", "llama").Object, "spec", "baseURL")
	if got != "http://vllm.models.svc:8000/v1" {
		t.Fatalf("baseURL %q", got)
	}
	if _, found, _ := unstructured.NestedString(find(t, r.Objects, "AgentgatewayModel", "gemma").Object, "spec", "baseURL"); found {
		t.Fatal("a provider model got a baseURL")
	}
}

// A self-hosted server is a custom provider that lists the APIs it implements, so agentgateway converts the others
// (agentgateway v1.6.0 would otherwise send Responses requests a vLLM or Ollama server may not implement). Without
// spec.formats: Chat Completions and Embeddings, which every OpenAI-compatible server implements.
func TestSelfHostedModelIsCustomProviderWithItsFormats(t *testing.T) {
	plain := model("llama", "meta/llama-3-8b")
	plain.Spec.Provider, plain.Spec.BaseURL = "OpenAI", "http://vllm.models.svc:8000/v1"
	rich := model("qwen", "qwen/qwen3")
	rich.Spec.Provider, rich.Spec.BaseURL, rich.Spec.Formats = "OpenAI", "http://litellm:4000/v1", []v1.ModelFormat{"Completions", "Responses", "Messages"}
	r := Compile([]v1.Model{plain, rich, model("gemma", "google/gemma:free")}, nil, nil, route)
	for name, want := range map[string][]string{"llama": {"Completions", "Embeddings"}, "qwen": {"Completions", "Responses", "Messages"}} {
		o := find(t, r.Objects, "AgentgatewayModel", name).Object
		if p, _, _ := unstructured.NestedString(o, "spec", "provider"); p != "Custom" {
			t.Errorf("%s provider %q", name, p)
		}
		fs, _, _ := unstructured.NestedSlice(o, "spec", "custom", "formats")
		var got []string
		for _, f := range fs {
			got = append(got, f.(map[string]any)["type"].(string))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s formats %v, want %v", name, got, want)
		}
		if b, _, _ := unstructured.NestedString(o, "spec", "baseURL"); b == "" {
			t.Errorf("%s lost its baseURL", name)
		}
	}
	g := find(t, r.Objects, "AgentgatewayModel", "gemma").Object
	if p, _, _ := unstructured.NestedString(g, "spec", "provider"); p == "Custom" {
		t.Errorf("a provider model became %q", p)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(g, "spec", "custom"); found {
		t.Error("a provider model got custom settings")
	}
}

// An AccessRule grants a router like a Model; naming it is not an unknown model.
func TestRuleNamingRouterIsKnown(t *testing.T) {
	r := Compile([]v1.Model{model("gemma", "google/gemma")}, nil,
		[]v1.AccessRule{rule("eng-auto", []string{"eng"}, false, "gemma", "auto")}, route, "auto")
	if c := r.RuleConditions["eng-auto"]; c.Status != metav1.ConditionTrue {
		t.Fatalf("rule naming a router: %s %s", c.Reason, c.Message)
	}
	if a := r.Access["auto"]; len(a.Groups) != 1 || a.Groups[0] != "eng" {
		t.Fatalf("router access %+v", a)
	}
	if _, ok := byName(r.Objects)["auto"]; ok {
		t.Fatal("an AgentgatewayModel was generated for a router name")
	}
}
