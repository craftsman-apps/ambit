package controller

import (
	"fmt"
	"github.com/craftsman-apps/ambit/internal/identity"
	"slices"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/routerconfig"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func router(name string, typ string, def string, routes ...v1.RouterRoute) v1.ModelRouter {
	return v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system", UID: types.UID("uid-r-" + name)},
		Spec: v1.ModelRouterSpec{Default: def, Classifier: v1.RouterClassifier{Type: typ}, Routes: routes}}
}

// targets returns the virtual model's (when, modelRef) pairs.
func targets(t *testing.T, o *unstructured.Unstructured) [][2]string {
	t.Helper()
	ts, _, _ := unstructured.NestedSlice(o.Object, "spec", "virtualModel", "conditional", "targets")
	var out [][2]string
	for _, x := range ts {
		m := x.(map[string]any)
		ref, _, _ := unstructured.NestedString(m, "modelRef", "name")
		w, _ := m["when"].(string)
		out = append(out, [2]string{w, ref})
	}
	return out
}

func compileWithRouters(models []v1.Model, rules []v1.AccessRule, routers ...v1.ModelRouter) (Result, map[string]metav1.Condition) {
	res, conds, _ := compileLLM(models, rules, ClassifierInput{}, routers...)
	return res, conds
}

func compileLLM(models []v1.Model, rules []v1.AccessRule, ci ClassifierInput, routers ...v1.ModelRouter) (Result, map[string]metav1.Condition, routerconfig.Config) {
	var names []string
	for _, r := range routers {
		names = append(names, r.Name)
	}
	res := Compile(models, nil, rules, route, names...)
	conds, cfg := CompileRouters(routers, models, &res, route, nil, ci)
	return res, conds, cfg
}

var (
	catalog = []v1.Model{model("gemma", "google/gemma"), model("sonnet", "anthropic/sonnet"), model("pro", "google/pro")}
	grants  = []v1.AccessRule{
		rule("auto-users", []string{"eng", "ml"}, false, "auto"),
		rule("all-gemma", nil, true, "gemma"),
		rule("ml-sonnet", []string{"ml"}, false, "sonnet"),
		rule("ml-pro", []string{"ml"}, false, "pro"),
	}
)

const (
	pathOK  = `request.path == "/v1/chat/completions"`
	textOf  = `llmRequest.messages.filter(m, m.role == "user")[size(llmRequest.messages.filter(m, m.role == "user")) - 1].content`
	autoCEL = identity.PrincipalGroups + `.exists(g, g in ["eng", "ml"])`
	mlCEL   = identity.PrincipalGroups + `.exists(g, g in ["ml"])`
)

// The lines in route order, each: router access, path, condition, target access; the default last.
func TestRulesRouterLines(t *testing.T) {
	res, conds := compileWithRouters(catalog, grants, router("auto", "Rules", "gemma",
		v1.RouterRoute{Model: "sonnet", Keywords: []string{"Python", "traceback"}},
		v1.RouterRoute{Model: "pro", MinBytes: ptr.To[int64](2000), MaxBytes: ptr.To[int64](90000)}))
	if c := conds["auto"]; c.Status != metav1.ConditionTrue {
		t.Fatalf("condition %s: %s", c.Reason, c.Message)
	}
	o := byName(res.Objects)["router-auto"]
	if o == nil {
		t.Fatal("no virtual model router-auto")
	}
	if m, _, _ := unstructured.NestedString(o.Object, "spec", "match", "model"); m != "auto" {
		t.Fatalf("match.model %q", m)
	}
	want := [][2]string{
		{`(` + autoCEL + `) && ` + pathOK + ` && ([` + textOf + `].exists(t, type(t) == string && [t.lowerAscii()].exists(l, l.contains("python") || l.contains("traceback")))) && (` + mlCEL + `)`, "sonnet"},
		{`(` + autoCEL + `) && ` + pathOK + ` && ([` + textOf + `].exists(t, type(t) == string && size(t) >= 2000 && size(t) <= 90000)) && (` + mlCEL + `)`, "pro"},
		{`(` + autoCEL + `) && (true)`, "gemma"},
	}
	got := targets(t, o)
	if len(got) != len(want) {
		t.Fatalf("%d lines, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %v\nwant %v", i, got[i], want[i])
		}
	}
}

func TestNoneIgnoresRoutes(t *testing.T) {
	res, _ := compileWithRouters(catalog, grants, router("auto", "None", "gemma", v1.RouterRoute{Model: "sonnet", Keywords: []string{"x"}}))
	if got := targets(t, byName(res.Objects)["router-auto"]); len(got) != 1 || got[0][1] != "gemma" {
		t.Fatalf("type None: %v", got)
	}
}

func TestKeywordQuoting(t *testing.T) {
	res, _ := compileWithRouters(catalog, grants, router("auto", "Rules", "gemma", v1.RouterRoute{Model: "sonnet", Keywords: []string{`say "hi"\n`}}))
	w := targets(t, byName(res.Objects)["router-auto"])[0][0]
	if !strings.Contains(w, `l.contains("say \"hi\"\\n")`) {
		t.Fatalf("keyword not quoted: %s", w)
	}
}

// A target refused to a dollar-capped group as unpriced: its line carries the refusal, so that group falls through.
func TestUnpricedTargetFallsThrough(t *testing.T) {
	names := []string{"auto"}
	res := Compile(catalog, nil, grants, route, names...)
	RefuseUnpriced(&res, catalog, map[string]v1.ModelPrice{"google/gemma": {}, "google/pro": {}}, []string{"ml"})
	CompileRouters([]v1.ModelRouter{router("auto", "Rules", "gemma", v1.RouterRoute{Model: "sonnet", Keywords: []string{"x"}})}, catalog, &res, route, nil, ClassifierInput{})
	w := targets(t, byName(res.Objects)["router-auto"])[0][0]
	if !strings.HasSuffix(w, `(`+`(`+mlCEL+`) && !`+identity.PrincipalGroups+`.exists(g, g in ["ml"])`+`)`) {
		t.Fatalf("unpriced refusal missing from the line: %s", w)
	}
}

func TestNameTaken(t *testing.T) {
	wild := model("wild", "gpt-*")
	for name, c := range map[string]struct {
		models []v1.Model
		r      v1.ModelRouter
	}{
		"exact":    {catalog, func() v1.ModelRouter { r := router("auto", "None", "gemma"); r.Spec.Model = "google/gemma"; return r }()},
		"wildcard": {append(slices.Clone(catalog), wild), func() v1.ModelRouter { r := router("auto", "None", "gemma"); r.Spec.Model = "gpt-auto"; return r }()},
		"object":   {append(slices.Clone(catalog), model("router-auto", "x/y")), router("auto", "None", "gemma")},
	} {
		res, conds := compileWithRouters(c.models, grants, c.r)
		obj := byName(res.Objects)["router-auto"] // for "object" this is the Model's own AgentgatewayModel
		if conds["auto"].Reason != "NameTaken" || (obj != nil && obj.GetLabels()[RouterLabel] != "") {
			t.Errorf("%s: reason %s, router object generated %v", name, conds["auto"].Reason, obj != nil && obj.GetLabels()[RouterLabel] != "")
		}
	}
	// two routers with one wire name: the first by name keeps it
	a, b := router("a", "None", "gemma"), router("b", "None", "gemma")
	a.Spec.Model, b.Spec.Model = "auto", "auto"
	_, conds := compileWithRouters(catalog, append(slices.Clone(grants), rule("ab", []string{"eng"}, false, "a", "b")), a, b)
	if conds["a"].Status != metav1.ConditionTrue || conds["b"].Reason != "NameTaken" {
		t.Fatalf("two routers: a %s, b %s", conds["a"].Reason, conds["b"].Reason)
	}
}

func TestAmbiguousTarget(t *testing.T) {
	r := router("auto", "Rules", "gemma", v1.RouterRoute{Model: "sonnet", Keywords: []string{"x"}})
	for name, extra := range map[string]v1.Model{
		"same wire name":   model("sonnet-eu", "anthropic/sonnet"),
		"wildcard overlap": model("anthropic-all", "anthropic/*"),
	} {
		if _, conds := compileWithRouters(append(slices.Clone(catalog), extra), grants, r); conds["auto"].Reason != "AmbiguousTarget" {
			t.Errorf("%s: got %s", name, conds["auto"].Reason)
		}
	}
	wild := router("auto", "Rules", "gemma", v1.RouterRoute{Model: "any-gpt", Keywords: []string{"x"}})
	if _, conds := compileWithRouters(append(slices.Clone(catalog), model("any-gpt", "gpt-*")), grants, wild); conds["auto"].Reason != "AmbiguousTarget" {
		t.Errorf("wildcard target: got %s", conds["auto"].Reason)
	}
}

func TestUnknownRouteModelIsDropped(t *testing.T) {
	res, conds := compileWithRouters(catalog, grants, router("auto", "Rules", "gemma",
		v1.RouterRoute{Model: "missing", Keywords: []string{"x"}}, v1.RouterRoute{Model: "sonnet", Keywords: []string{"y"}}))
	if c := conds["auto"]; c.Reason != "UnknownModel" || !strings.Contains(c.Message, "missing") {
		t.Fatalf("condition %s %s", c.Reason, c.Message)
	}
	if got := targets(t, byName(res.Objects)["router-auto"]); len(got) != 2 || got[0][1] != "sonnet" {
		t.Fatalf("lines %v", got)
	}
	_, conds = compileWithRouters(catalog, grants, router("auto", "None", "nope"))
	if conds["auto"].Reason != "UnknownDefault" {
		t.Fatalf("unknown default: %s", conds["auto"].Reason)
	}
}

// A category route with type Rules is ignored as a whole, even when it also has keywords.
func TestCategoryNeedsClassifier(t *testing.T) {
	res, conds := compileWithRouters(catalog, grants, router("auto", "Rules", "gemma",
		v1.RouterRoute{Model: "sonnet", Category: "coding", Keywords: []string{"python"}},
		v1.RouterRoute{Model: "pro", Keywords: []string{"long"}}))
	if conds["auto"].Reason != "CategoryNeedsClassifier" {
		t.Fatalf("got %s", conds["auto"].Reason)
	}
	if got := targets(t, byName(res.Objects)["router-auto"]); len(got) != 2 || got[0][1] != "pro" {
		t.Fatalf("lines %v", got)
	}
}

func TestTooManyKeywords(t *testing.T) {
	kw := make([]string, 33)
	for i := range kw {
		kw[i] = fmt.Sprint("k", i)
	}
	res, conds := compileWithRouters(catalog, grants, router("auto", "Rules", "gemma", v1.RouterRoute{Model: "sonnet", Keywords: kw}))
	if conds["auto"].Reason != "TooManyRoutes" || byName(res.Objects)["router-auto"] != nil {
		t.Fatalf("got %s", conds["auto"].Reason)
	}
}

// No group may use both the router and its default: every request would get 400. Generated anyway, reported.
func TestDefaultNotAllowed(t *testing.T) {
	rules := []v1.AccessRule{rule("auto-users", []string{"eng"}, false, "auto"), rule("ml-sonnet", []string{"ml"}, false, "sonnet")}
	res, conds := compileWithRouters(catalog, rules, router("auto", "None", "sonnet"))
	if conds["auto"].Reason != "DefaultNotAllowed" || byName(res.Objects)["router-auto"] == nil {
		t.Fatalf("got %s", conds["auto"].Reason)
	}
}

// A Model with the router's object name would share its AccessRules (rules name objects): a grant meant for the
// router would also open the Model directly. The router is refused instead (final review, 2026-10-06).
func TestModelWithRouterObjectNameIsNameTaken(t *testing.T) {
	r := router("auto", "None", "gemma")
	r.Spec.Model = "smart"
	res, conds := compileWithRouters(append(slices.Clone(catalog), model("auto", "meta/llama")), grants, r)
	if conds["auto"].Reason != "NameTaken" || !strings.Contains(conds["auto"].Message, "AccessRules") {
		t.Fatalf("got %s: %s", conds["auto"].Reason, conds["auto"].Message)
	}
	if o := byName(res.Objects)["router-auto"]; o != nil {
		t.Fatal("router served although a Model has its object name")
	}
}

// The default's final access includes the unpriced refusal: when every group that may use the router is
// refused the default (unpriced, dollar-capped), every unmatched request gets 400 (final review, 2026-10-06).
func TestDefaultNotAllowedCountsUnpricedRefusal(t *testing.T) {
	rules := []v1.AccessRule{rule("ml-auto", []string{"ml"}, false, "auto"), rule("ml-sonnet", []string{"ml"}, false, "sonnet")}
	res := Compile(catalog, nil, rules, route, "auto")
	refused := RefuseUnpriced(&res, catalog, map[string]v1.ModelPrice{"google/gemma": {}, "google/pro": {}}, []string{"ml"})
	conds, _ := CompileRouters([]v1.ModelRouter{router("auto", "None", "sonnet")}, catalog, &res, route, refused, ClassifierInput{})
	if conds["auto"].Reason != "DefaultNotAllowed" {
		t.Fatalf("got %s", conds["auto"].Reason)
	}
}

// Several warnings: the first sets the reason, the message names all of them.
func TestRouterWarningsAreAllShown(t *testing.T) {
	rules := []v1.AccessRule{rule("auto-users", []string{"eng"}, false, "auto"), rule("ml-sonnet", []string{"ml"}, false, "sonnet")}
	_, conds := compileWithRouters(catalog, rules, router("auto", "Rules", "sonnet", v1.RouterRoute{Model: "missing", Keywords: []string{"x"}}))
	c := conds["auto"]
	if c.Reason != "UnknownModel" || !strings.Contains(c.Message, "missing") || !strings.Contains(c.Message, "no group or user may use both") {
		t.Fatalf("got %s: %s", c.Reason, c.Message)
	}
}

var local = v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "ambit-system"},
	Spec: v1.ModelSpec{Provider: "OpenAI", Model: "qwen3-0.6b", BaseURL: "http://vllm:8000/v1", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-local"}}}
var installed = ClassifierInput{Installed: true, HasKey: map[string]bool{"local": true}}

func llmRouter(name string, cats ...string) v1.ModelRouter {
	r := router(name, "LLM", "gemma", v1.RouterRoute{Model: "sonnet", Category: cats[0]}, v1.RouterRoute{Model: "sonnet", Keywords: []string{"python"}})
	r.Spec.Classifier.LLM = &v1.RouterLLM{Model: "local"}
	for _, c := range cats {
		r.Spec.Classifier.Categories = append(r.Spec.Classifier.Categories, v1.RouterCategory{Name: c, Description: c + " prompts"})
	}
	return r
}

func TestLLMRouterCategoryLine(t *testing.T) {
	models := append(slices.Clone(catalog), local)
	res, conds, cfg := compileLLM(models, grants, installed, llmRouter("auto", "coding"))
	if conds["auto"].Status != metav1.ConditionTrue {
		t.Fatalf("%v", conds["auto"])
	}
	if len(cfg.Routers) != 1 {
		t.Fatalf("config %+v", cfg)
	}
	r := cfg.Routers[0]
	if cfg.Hash != routerconfig.HashOf(cfg.Routers) || r.LLM.Endpoint != "http://vllm:8000/v1" || r.LLM.Model != "qwen3-0.6b" ||
		r.LLM.CredentialKey != "local" || r.TimeoutMillis != 1500 || r.MaxBytes != 8000 || r.RatePerMinute != 600 || !slices.Equal(r.Groups, []string{"eng", "ml"}) {
		t.Fatalf("config %+v", cfg)
	}
	ts := targets(t, byName(res.Objects)["router-auto"])
	want := `request.headers["x-ambit-config"] == "` + cfg.Hash + `" && request.headers["x-ambit-route"] == "coding"`
	if len(ts) != 3 || !strings.Contains(ts[0][0], want) || strings.Contains(ts[1][0], "x-ambit") || ts[2][1] != "gemma" {
		t.Fatalf("lines %q", ts)
	}
}

func TestLLMRouterValidation(t *testing.T) {
	anthropic := model("claude", "anthropic/claude-x")
	anthropic.Spec.Provider = "Anthropic"
	withAnthropic := llmRouter("auto", "coding")
	withAnthropic.Spec.Classifier.LLM.Model = "claude"
	undeclared := llmRouter("auto", "coding")
	undeclared.Spec.Routes[0].Category = "maths"
	base := append(slices.Clone(catalog), local)
	noChat := local // ambit-router calls /chat/completions itself, without the gateway's conversion
	noChat.Spec.Formats = []v1.ModelFormat{"Messages", "Responses"}
	azure := model("gpt", "gpt-4.1")
	azure.Spec.Provider, azure.Spec.Azure = "Azure", &v1.AzureSettings{ResourceName: "acme-openai", ResourceType: "OpenAI"}
	withAzure := llmRouter("auto", "coding")
	withAzure.Spec.Classifier.LLM.Model = "gpt"
	bedrock := model("claude-role", "anthropic.claude-sonnet-4-5-20250929-v1:0")
	bedrock.Spec.Provider, bedrock.Spec.CredentialsRef = "Bedrock", nil
	withBedrock := llmRouter("auto", "coding")
	withBedrock.Spec.Classifier.LLM.Model = "claude-role"
	vx := model("gemini-vx", "gemini-2.5-pro")
	vx.Spec.Provider, vx.Spec.VertexAI = "VertexAI", &v1.VertexAISettings{ProjectID: "acme-ml"}
	withVertex := llmRouter("auto", "coding")
	withVertex.Spec.Classifier.LLM.Model = "gemini-vx"
	noRef := local
	noRef.Spec.CredentialsRef = nil // invalid in the API; the message must still not panic
	for name, tc := range map[string]struct {
		r      v1.ModelRouter
		models []v1.Model
		ci     ClassifierInput
		reason string
		inCfg  bool
	}{
		"missing classifier model": {llmRouter("auto", "coding"), catalog, installed, "ClassifierModelInvalid", false},
		"no openai endpoint":       {withAnthropic, append(slices.Clone(base), anthropic), ClassifierInput{Installed: true, HasKey: map[string]bool{"claude": true}}, "ClassifierModelInvalid", false},
		"key secret missing":       {llmRouter("auto", "coding"), base, ClassifierInput{Installed: true}, "ClassifierModelInvalid", false},
		"formats without chat":     {llmRouter("auto", "coding"), append(slices.Clone(catalog), noChat), installed, "ClassifierModelInvalid", false},
		"category not declared":    {undeclared, base, installed, "UnknownCategory", true},
		"azure classifier":         {withAzure, append(slices.Clone(base), azure), ClassifierInput{Installed: true, HasKey: map[string]bool{"gpt": true}}, "ClassifierModelInvalid", false},
		"bedrock classifier":       {withBedrock, append(slices.Clone(base), bedrock), installed, "ClassifierModelInvalid", false},
		"vertexai classifier":      {withVertex, append(slices.Clone(base), vx), ClassifierInput{Installed: true, HasKey: map[string]bool{"gemini-vx": true}}, "ClassifierModelInvalid", false},
		"no credentialsRef":        {llmRouter("auto", "coding"), append(slices.Clone(catalog), noRef), ClassifierInput{Installed: true}, "ClassifierModelInvalid", false},
		"not installed":            {llmRouter("auto", "coding"), base, ClassifierInput{}, "ClassifierNotInstalled", false},
	} {
		res, conds, cfg := compileLLM(tc.models, grants, tc.ci, tc.r)
		if conds["auto"].Reason != tc.reason || (len(cfg.Routers) == 1) != tc.inCfg {
			t.Errorf("%s: %s %s; configured=%v", name, conds["auto"].Reason, conds["auto"].Message, len(cfg.Routers) == 1)
		}
		if tc.reason == "ClassifierNotInstalled" { // served, category routes ignored
			if ts := targets(t, byName(res.Objects)["router-auto"]); len(ts) != 2 {
				t.Errorf("not installed: keyword line and default only, got %q", ts)
			}
		}
	}
}

func TestCategoryConflict(t *testing.T) {
	a, b := llmRouter("a", "coding"), llmRouter("b", "coding")
	b.Spec.Model = "auto-b"
	b.Spec.Classifier.Categories[0].Description = "something else"
	ab := append(slices.Clone(grants), rule("ab-users", []string{"eng"}, false, "a", "b")) // routers need users, or DefaultNotAllowed
	_, conds, cfg := compileLLM(append(slices.Clone(catalog), local), ab, installed, a, b)
	if conds["b"].Reason != "CategoryConflict" || conds["a"].Status != metav1.ConditionTrue || len(cfg.Routers) != 1 {
		t.Fatalf("a=%v b=%v", conds["a"], conds["b"])
	}
}

// Azure and Bedrock Models are ordinary route targets and defaults.
func TestCloudModelsAsRouteTargets(t *testing.T) {
	azure := model("gpt", "gpt-4.1")
	azure.Spec.Provider, azure.Spec.Azure = "Azure", &v1.AzureSettings{ResourceName: "acme-openai", ResourceType: "OpenAI"}
	bedrock := model("claude-role", "anthropic.claude-sonnet-4-5-20250929-v1:0")
	bedrock.Spec.Provider, bedrock.Spec.CredentialsRef = "Bedrock", nil
	vx := model("gemini-vx", "gemini-2.5-pro")
	vx.Spec.Provider, vx.Spec.CredentialsRef = "VertexAI", nil
	vx.Spec.VertexAI = &v1.VertexAISettings{ProjectID: "acme-ml"}
	rules := append(slices.Clone(grants), rule("all-cloud", nil, true, "gpt", "claude-role", "gemini-vx", "auto2"))
	res, conds, _ := compileLLM([]v1.Model{azure, bedrock, vx}, rules, ClassifierInput{},
		router("auto", "Rules", "claude-role", v1.RouterRoute{Model: "gpt", Keywords: []string{"python"}},
			v1.RouterRoute{Model: "gemini-vx", Keywords: []string{"gemini"}}),
		router("auto2", "Rules", "gemini-vx", v1.RouterRoute{Model: "gpt", Keywords: []string{"python"}}))
	for _, r := range []string{"auto", "auto2"} {
		if conds[r].Status != metav1.ConditionTrue {
			t.Fatalf("%s: %s %s", r, conds[r].Reason, conds[r].Message)
		}
	}
	ts := targets(t, byName(res.Objects)["router-auto"])
	if len(ts) != 3 || ts[0][1] != "gpt" || ts[1][1] != "gemini-vx" || ts[2][1] != "claude-role" {
		t.Fatalf("lines %q", ts)
	}
	if ts := targets(t, byName(res.Objects)["router-auto2"]); len(ts) != 2 || ts[1][1] != "gemini-vx" {
		t.Fatalf("auto2 lines %q", ts)
	}
}

// Audit mode (#81): every AccessRule decision is allowed. Models get `true` (the unpriced refusal stays: it guards a
// Budget), the MCP access policy gets `true`, router lines keep only their route condition and the target's
// unpriced refusal. The enforce objects the caller compiled are not changed.
func TestAuditModeAuthorization(t *testing.T) {
	models := []v1.Model{model("gemma", "google/gemma"), model("sonnet", "anthropic/sonnet"), model("pro", "google/pro")}
	models[0].Spec.Price = &v1.ModelPrice{InputPerMillionTokens: "1", OutputPerMillionTokens: "1"}
	models[1].Spec.Price = models[0].Spec.Price // pro has no price: refused to the dollar-capped group
	rules := append(slices.Clone(grants), toolRule("mcp", []string{"ml"}, false, v1.ServerAccess{Name: "time"}))
	routers := []v1.ModelRouter{router("auto", "Rules", "gemma",
		v1.RouterRoute{Model: "sonnet", Keywords: []string{"Python"}}, v1.RouterRoute{Model: "pro", Keywords: []string{"Go"}})}
	res := Compile(models, []v1.Server{server("time", "stdio")}, rules, route, "auto")
	refused := RefuseUnpriced(&res, models, pricingByModel(models), []string{"capped"})
	CompileRouters(routers, models, &res, route, refused, ClassifierInput{})
	enforce := make([]string, len(res.Objects))
	for i, o := range res.Objects {
		b, _ := o.MarshalJSON()
		enforce[i] = string(b)
	}

	audit := RelaxForAudit(res, refused, func(a *Result) { CompileRouters(routers, models, a, route, refused, ClassifierInput{}) })

	for i, o := range res.Objects {
		if b, _ := o.MarshalJSON(); string(b) != enforce[i] {
			t.Fatalf("RelaxForAudit changed the enforce object %s", o.GetName())
		}
	}
	if len(audit) != len(res.Objects) {
		t.Fatalf("%d objects in audit mode, %d in enforce mode", len(audit), len(res.Objects))
	}
	deny := "!" + identity.PrincipalGroups + `.exists(g, g in ["capped"])`
	objs := byName(audit)
	exprOf := func(name string) []string {
		e, _, _ := unstructured.NestedStringSlice(objs[name].Object, "spec", "policies", "authorization", "policy", "matchExpressions")
		return e
	}
	if got := exprOf("gemma"); !slices.Equal(got, []string{"true"}) {
		t.Errorf("gemma %q, want [true]", got)
	}
	if got := exprOf("pro"); !slices.Equal(got, []string{"(true) && " + deny}) {
		t.Errorf("pro %q, want the unpriced refusal kept", got)
	}
	p := objs[MCPBackendName+"-access"]
	if e, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "backend", "mcp", "authorization", "policy", "matchExpressions"); !slices.Equal(e, []string{"true"}) {
		t.Errorf("MCP access %q, want [true]", e)
	}
	cond := func(i int) string { return routeCondition(routers[0].Spec.Routes[i], "") }
	want := [][2]string{
		{pathOK + ` && (` + cond(0) + `)`, "sonnet"},
		{pathOK + ` && (` + cond(1) + `) && ((true) && ` + deny + `)`, "pro"},
		{"true", "gemma"},
	}
	if got := targets(t, objs["router-auto"]); !slices.Equal(got, want) {
		t.Errorf("router lines\n got %q\nwant %q", got, want)
	}
}
