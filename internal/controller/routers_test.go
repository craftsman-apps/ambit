package controller

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
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
	var names []string
	for _, r := range routers {
		names = append(names, r.Name)
	}
	res := Compile(models, nil, rules, route, names...)
	conds := CompileRouters(routers, models, &res, route, nil)
	return res, conds
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
	autoCEL = `jwt.groups.exists(g, g in ["eng", "ml"])`
	mlCEL   = `jwt.groups.exists(g, g in ["ml"])`
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
	CompileRouters([]v1.ModelRouter{router("auto", "Rules", "gemma", v1.RouterRoute{Model: "sonnet", Keywords: []string{"x"}})}, catalog, &res, route, nil)
	w := targets(t, byName(res.Objects)["router-auto"])[0][0]
	if !strings.HasSuffix(w, `(`+`(`+mlCEL+`) && !jwt.groups.exists(g, g in ["ml"])`+`)`) {
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
	conds := CompileRouters([]v1.ModelRouter{router("auto", "None", "sonnet")}, catalog, &res, route, refused)
	if conds["auto"].Reason != "DefaultNotAllowed" {
		t.Fatalf("got %s", conds["auto"].Reason)
	}
}

// Several warnings: the first sets the reason, the message names all of them.
func TestRouterWarningsAreAllShown(t *testing.T) {
	rules := []v1.AccessRule{rule("auto-users", []string{"eng"}, false, "auto"), rule("ml-sonnet", []string{"ml"}, false, "sonnet")}
	_, conds := compileWithRouters(catalog, rules, router("auto", "Rules", "sonnet", v1.RouterRoute{Model: "missing", Keywords: []string{"x"}}))
	c := conds["auto"]
	if c.Reason != "UnknownModel" || !strings.Contains(c.Message, "missing") || !strings.Contains(c.Message, "no group may use both") {
		t.Fatalf("got %s: %s", c.Reason, c.Message)
	}
}
