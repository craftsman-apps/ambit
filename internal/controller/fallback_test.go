package controller

import (
	"slices"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func withEndpoints(m v1.Model, urls ...string) v1.Model {
	m.Spec.Provider, m.Spec.Model = "OpenAI", m.Name
	m.Spec.BaseURL, m.Spec.CredentialsRef = "http://first.svc:8000/v1", &v1.SecretRef{Name: "ambit-llm-first"}
	for i, u := range urls {
		m.Spec.AdditionalEndpoints = append(m.Spec.AdditionalEndpoints,
			v1.ModelEndpoint{BaseURL: u, CredentialsRef: &v1.SecretRef{Name: "ambit-llm-ep" + string(rune('1'+i))}})
	}
	return m
}

func withFallback(m v1.Model, names ...string) v1.Model {
	m.Spec.Fallback = names
	return m
}

func virtualOf(t *testing.T, objs map[string]*unstructured.Unstructured, name string) []map[string]any {
	t.Helper()
	o := objs[name]
	if o == nil {
		t.Fatalf("no object %s in %v", name, keys(objs))
	}
	ts, _, _ := unstructured.NestedSlice(o.Object, "spec", "virtualModel", "failover", "targets")
	var out []map[string]any
	for _, x := range ts {
		out = append(out, x.(map[string]any))
	}
	return out
}

func keys(m map[string]*unstructured.Unstructured) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func tgt(t map[string]any) (string, int64) {
	return t["modelRef"].(map[string]any)["name"].(string), t["priority"].(int64)
}

func TestModelWithoutFallbackFieldsCompilesAsBefore(t *testing.T) {
	r := compileModels([]v1.Model{model("gemma", "google/gemma:free")}, nil, route)
	if len(r.Objects) != 1 {
		t.Fatalf("want one object, got %v", keys(byName(r.Objects)))
	}
	o := r.Objects[0]
	if _, ok, _ := unstructured.NestedString(o.Object, "spec", "visibility"); ok {
		t.Fatal("a plain model must stay public")
	}
	if _, ok, _ := unstructured.NestedMap(o.Object, "spec", "policies", "health"); ok {
		t.Fatal("a plain model gets no health block")
	}
	if got := RetryPolicy(r.Objects, "ambit-system", "ambit-llm"); got != nil {
		t.Fatalf("no retry policy without failover, got %v", got.Object)
	}
}

func TestAdditionalEndpointsShareTheWireNameAtOnePriority(t *testing.T) {
	m := withEndpoints(model("gpt-x", "gpt-x"), "http://second.svc:8000/v1", "http://third.svc:8000/v1")
	r := compileModels([]v1.Model{m}, []v1.AccessRule{rule("eng", []string{"eng"}, false, "gpt-x")}, route)
	objs := byName(r.Objects)
	if want := []string{"gpt-x", "gpt-x-ep1", "gpt-x-ep2", "gpt-x-failover"}; !slices.Equal(keys(objs), want) {
		t.Fatalf("objects %v, want %v", keys(objs), want)
	}
	for _, n := range []string{"gpt-x", "gpt-x-ep1", "gpt-x-ep2"} {
		o := objs[n]
		if v, _, _ := unstructured.NestedString(o.Object, "spec", "visibility"); v != "Internal" {
			t.Fatalf("%s visibility %q, want Internal", n, v)
		}
		if got, _, _ := unstructured.NestedString(o.Object, "spec", "match", "model"); got != "gpt-x" {
			t.Fatalf("%s matches %q", n, got)
		}
		if expr(t, o) != expr(t, objs["gpt-x"]) {
			t.Fatalf("%s has another access expression", n)
		}
		if _, ok, _ := unstructured.NestedMap(o.Object, "spec", "policies", "health"); !ok {
			t.Fatalf("%s has no health block", n)
		}
	}
	if got, _, _ := unstructured.NestedString(objs["gpt-x-ep1"].Object, "spec", "baseURL"); got != "http://second.svc:8000/v1" {
		t.Fatalf("ep1 baseURL %q", got)
	}
	if got, _, _ := unstructured.NestedString(objs["gpt-x-ep2"].Object, "spec", "custom", "backendRef", "name"); got != "" {
		t.Fatalf("unexpected backendRef %q", got)
	}
	if got, _, _ := unstructured.NestedString(objs["gpt-x"].Object, "spec", "baseURL"); got != "http://first.svc:8000/v1" {
		t.Fatalf("primary baseURL %q", got)
	}
	vm := objs["gpt-x-failover"]
	if got, _, _ := unstructured.NestedString(vm.Object, "spec", "match", "model"); got != "gpt-x" {
		t.Fatalf("virtual model matches %q", got)
	}
	if _, ok, _ := unstructured.NestedMap(vm.Object, "spec", "policies"); ok {
		t.Fatal("agentgateway refuses policies on a virtual model")
	}
	var got []string
	for _, tg := range virtualOf(t, objs, "gpt-x-failover") {
		n, p := tgt(tg)
		got = append(got, n+"@"+string(rune('0'+p)))
	}
	if want := []string{"gpt-x@0", "gpt-x-ep1@0", "gpt-x-ep2@0"}; !slices.Equal(got, want) {
		t.Fatalf("targets %v, want %v", got, want)
	}
	if len(vm.GetOwnerReferences()) != 1 || vm.GetOwnerReferences()[0].Name != "gpt-x" || vm.GetLabels()[FallbackLabel] != "gpt-x" {
		t.Fatalf("owner or label missing: %v %v", vm.GetOwnerReferences(), vm.GetLabels())
	}
}

func TestFallbackModelsFollowInOrderAtLowerPriority(t *testing.T) {
	a, b, c := model("a", "model-a"), model("b", "model-b"), model("c", "model-c")
	a = withFallback(a, "b", "c")
	r := compileModels([]v1.Model{a, b, c}, nil, route)
	objs := byName(r.Objects)
	var got []string
	for _, tg := range virtualOf(t, objs, "a-failover") {
		n, p := tgt(tg)
		got = append(got, n+"@"+string(rune('0'+p)))
	}
	if want := []string{"a@0", "a-fb1@1", "a-fb2@2"}; !slices.Equal(got, want) {
		t.Fatalf("targets %v, want %v", got, want)
	}
	for _, n := range []string{"a", "a-fb1", "a-fb2"} {
		h, ok, _ := unstructured.NestedMap(objs[n].Object, "spec", "policies", "health")
		if !ok {
			t.Fatalf("%s has no health block", n)
		}
		if cond := h["unhealthyCondition"].(string); !strings.Contains(cond, "response.code == 429") || !strings.Contains(cond, ">= 500") {
			t.Fatalf("%s unhealthyCondition %q must list 5xx and 429", n, cond)
		}
		ev := h["eviction"].(map[string]any)
		if ev["consecutiveFailures"].(int64) != 1 || ev["duration"].(string) != "10s" {
			t.Fatalf("%s eviction %v", n, ev)
		}
	}
	if v, _, _ := unstructured.NestedString(objs["a"].Object, "spec", "visibility"); v != "Internal" {
		t.Fatalf("a must be Internal, got %q", v)
	}
	for _, n := range []string{"a-fb1", "a-fb2"} {
		if v, _, _ := unstructured.NestedString(objs[n].Object, "spec", "visibility"); v != "Internal" {
			t.Fatalf("%s is a target of the virtual model and must be Internal, got %q", n, v)
		}
	}
	for _, n := range []string{"b", "c"} {
		if _, ok, _ := unstructured.NestedString(objs[n].Object, "spec", "visibility"); ok {
			t.Fatalf("%s has no fallback of its own and stays public", n)
		}
		if _, ok, _ := unstructured.NestedMap(objs[n].Object, "spec", "policies", "health"); ok {
			t.Fatalf("%s is not a target of a virtual model; the copy is", n)
		}
	}
	if _, has := objs["b-failover"]; has {
		t.Fatal("b needs no virtual model")
	}
}

func TestFallbackUsesEveryEndpointOfTheFallbackModel(t *testing.T) {
	a := withFallback(model("a", "model-a"), "b")
	b := withEndpoints(model("b", "model-b"), "http://b2.svc:8000/v1")
	r := compileModels([]v1.Model{a, b}, nil, route)
	var got []string
	for _, tg := range virtualOf(t, byName(r.Objects), "a-failover") {
		n, p := tgt(tg)
		got = append(got, n+"@"+string(rune('0'+p)))
	}
	if want := []string{"a@0", "a-fb1@1", "a-fb1-ep1@1"}; !slices.Equal(got, want) {
		t.Fatalf("targets %v, want %v", got, want)
	}
}

func TestFallbackOfFallbackIsNotFollowed(t *testing.T) {
	a := withFallback(model("a", "model-a"), "b")
	b := withFallback(model("b", "model-b"), "c")
	r := compileModels([]v1.Model{a, b, model("c", "model-c")}, nil, route)
	objs := byName(r.Objects)
	if n := len(virtualOf(t, objs, "a-failover")); n != 2 {
		t.Fatalf("a-failover has %d targets, want 2 (a, b)", n)
	}
	if !hasNote(r, "a", "b has its own fallback") {
		t.Fatalf("want a note about b's fallback on a, got %v", r.Notes["a"])
	}
}

func hasNote(r Result, model, sub string) bool {
	for _, n := range r.Notes[model] {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestInvalidFallbacksAreDroppedWithANote(t *testing.T) {
	wild := model("w", "gpt-*")
	a := withFallback(model("a", "model-a"), "a", "missing", "w", "b")
	r := compileModels([]v1.Model{a, wild, model("b", "model-b")}, nil, route)
	var got []string
	for _, tg := range virtualOf(t, byName(r.Objects), "a-failover") {
		n, _ := tgt(tg)
		got = append(got, n)
	}
	if want := []string{"a", "a-fb1"}; !slices.Equal(got, want) {
		t.Fatalf("targets %v, want %v", got, want)
	}
	for _, sub := range []string{"no Model named missing", "cannot fall back to itself", "serves a wildcard"} {
		if !hasNote(r, "a", sub) {
			t.Errorf("missing note %q in %v", sub, r.Notes["a"])
		}
	}
}

func TestModelOnlyWithInvalidFallbacksStaysPlain(t *testing.T) {
	a := withFallback(model("a", "model-a"), "missing")
	r := compileModels([]v1.Model{a}, nil, route)
	if len(r.Objects) != 1 {
		t.Fatalf("want one object, got %v", keys(byName(r.Objects)))
	}
	if v, ok, _ := unstructured.NestedString(r.Objects[0].Object, "spec", "visibility"); ok {
		t.Fatalf("visibility %q: a model without a usable fallback must stay public", v)
	}
}

func TestWildcardModelCannotHaveEndpointsOrFallback(t *testing.T) {
	m := withEndpoints(model("w", "w"), "http://second.svc:8000/v1")
	m.Spec.Model = "gpt-*"
	r := compileModels([]v1.Model{m}, nil, route)
	if len(r.Objects) != 1 || !hasNote(r, "w", "wildcard") {
		t.Fatalf("want one plain object and a wildcard note, got %v %v", keys(byName(r.Objects)), r.Notes["w"])
	}
}

func TestEndpointWithoutBaseURLOnASelfHostedModelIsNotCompiled(t *testing.T) {
	m := withEndpoints(model("a", "a"), "http://second.svc:8000/v1")
	m.Spec.AdditionalEndpoints = append(m.Spec.AdditionalEndpoints, v1.ModelEndpoint{CredentialsRef: &v1.SecretRef{Name: "ambit-llm-open"}})
	r := compileModels([]v1.Model{m}, nil, route)
	if len(r.Objects) != 1 || !hasNote(r, "a", "public server") {
		t.Fatalf("want one plain object and a note, got %v %v", keys(byName(r.Objects)), r.Notes["a"])
	}
	// The fallback still works; only the endpoints are dropped.
	r = compileModels([]v1.Model{withFallback(m, "b"), model("b", "model-b")}, nil, route)
	got := byName(r.Objects)
	if got["a-ep1"] != nil || got["a-ep2"] != nil || len(virtualOf(t, got, "a-failover")) != 2 {
		t.Fatalf("endpoints compiled: %v", keys(got))
	}
}

// The fallback is reached through a copy that only people who may use both Models can use (#193).
func TestFallbackCopyIsOpenOnlyToPeopleWhoMayUseBothModels(t *testing.T) {
	a, b := withFallback(model("a", "model-a"), "b"), model("b", "model-b")
	rules := []v1.AccessRule{rule("eng", []string{"eng"}, false, "a"), rule("ops", []string{"ops"}, false, "b")}
	objs := byName(compileModels([]v1.Model{a, b}, rules, route).Objects)
	if got, want := expr(t, objs["a-fb1"]), `(`+CEL(Access{Groups: []string{"eng"}})+`) && (`+CEL(Access{Groups: []string{"ops"}})+`)`; got != want {
		t.Fatalf("copy expression %s, want %s", got, want)
	}
	if got := expr(t, objs["b"]); got != CEL(Access{Groups: []string{"ops"}}) {
		t.Fatalf("the fallback Model itself keeps its own rule, got %s", got)
	}
	// Open to everyone on one side: the copy is the other side's rule, unchanged.
	objs = byName(compileModels([]v1.Model{a, b}, []v1.AccessRule{rule("all", nil, true, "a"), rule("ops", []string{"ops"}, false, "b")}, route).Objects)
	if got, want := expr(t, objs["a-fb1"]), CEL(Access{Groups: []string{"ops"}}); got != want {
		t.Fatalf("copy expression %s, want %s", got, want)
	}
	// No rule on the primary: nobody may use its name, so nobody is answered under it by the fallback.
	objs = byName(compileModels([]v1.Model{a, b}, []v1.AccessRule{rule("ops", []string{"ops"}, false, "b")}, route).Objects)
	if got := expr(t, objs["a-fb1"]); !strings.HasPrefix(got, "(false) && ") {
		t.Fatalf("copy expression %s must refuse everyone", got)
	}
}

func TestFallbackCopyBelongsToThePrimaryAndKeepsTheFallbacksPriceRules(t *testing.T) {
	a, b := withFallback(model("a", "model-a"), "b"), withEndpoints(model("b", "model-b"), "http://b2.svc:8000/v1")
	res := compileModels([]v1.Model{a, b}, []v1.AccessRule{rule("all", nil, true, "a", "b")}, route)
	objs := byName(res.Objects)
	for _, n := range []string{"a-fb1", "a-fb1-ep1"} {
		o := objs[n]
		if o == nil {
			t.Fatalf("no %s in %v", n, keys(objs))
		}
		if o.GetLabels()[FallbackLabel] != "a" || o.GetLabels()[FallbackTargetLabel] != "a" || modelOf(o) != "b" {
			t.Fatalf("%s labels %v", n, o.GetLabels())
		}
		if refs := o.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "a" {
			t.Fatalf("%s owner %v, want the primary", n, refs)
		}
	}
	refused := RefuseUnpriced(&res, []v1.Model{a, b}, map[string]v1.ModelPrice{"model-a": {}}, []string{"eng"})
	if gs := refused["b"]; !slices.Equal(gs, []string{"eng"}) {
		t.Fatalf("refused %v, want b refused to eng", refused)
	}
	for _, n := range []string{"b", "b-ep1", "a-fb1", "a-fb1-ep1"} {
		if got := expr(t, objs[n]); !strings.Contains(got, `"eng"`) {
			t.Fatalf("%s is still open to eng: %s", n, got)
		}
	}
}

func TestAuditFieldsIgnoreFallbackCopies(t *testing.T) {
	rules := []v1.AccessRule{rule("eng", []string{"eng"}, false, "a", "b")}
	plain := compileModels([]v1.Model{model("a", "model-a"), model("b", "model-b")}, rules, route)
	fb := compileModels([]v1.Model{withFallback(model("a", "model-a"), "b"), model("b", "model-b")}, rules, route)
	f1, err1 := AuditFields(plain, 10)
	f2, err2 := AuditFields(fb, 10)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if !slices.EqualFunc(f1, f2, func(x, y any) bool { return x.(map[string]any)["expression"] == y.(map[string]any)["expression"] }) {
		t.Fatalf("a fallback changed the audit fields:\n%v\n%v", f1, f2)
	}
}

func TestGeneratedNameClashingWithAModelDisablesFallback(t *testing.T) {
	if r := compileModels([]v1.Model{withFallback(model("a", "model-a"), "b"), model("b", "model-b"), model("a-fb1-ep3", "model-x")}, nil, route); !hasNote(r, "a", "a-fb1-ep3") {
		t.Fatalf("a Model named like a copy of a fallback must disable the fallback: %v", r.Notes["a"])
	}
	a := withFallback(model("a", "model-a"), "b")
	r := compileModels([]v1.Model{a, model("b", "model-b"), model("a-failover", "model-x")}, nil, route)
	if _, has := byName(r.Objects)["a-failover"]; !has || len(virtualOf2(r, "a-failover")) != 0 {
		t.Fatalf("the Model named a-failover must stay a plain model")
	}
	if !hasNote(r, "a", "a-failover") {
		t.Fatalf("want a note about the name clash, got %v", r.Notes["a"])
	}
}

func virtualOf2(r Result, name string) []any {
	o := byName(r.Objects)[name]
	ts, _, _ := unstructured.NestedSlice(o.Object, "spec", "virtualModel", "failover", "targets")
	return ts
}

func TestFallbackNotOpenToThePrimarysAudienceWarns(t *testing.T) {
	a := withFallback(model("a", "model-a"), "b", "c")
	rules := []v1.AccessRule{
		rule("r1", []string{"eng", "ops"}, false, "a"),
		rule("r2", []string{"eng"}, false, "b"),
		rule("r3", nil, true, "c"),
	}
	r := compileModels([]v1.Model{a, model("b", "model-b"), model("c", "model-c")}, rules, route)
	if !hasNote(r, "a", "b is not open to ops") {
		t.Fatalf("want a coverage note for b, got %v", r.Notes["a"])
	}
	if hasNote(r, "a", "c is not open") {
		t.Fatalf("c is open to everyone signed in: %v", r.Notes["a"])
	}
	// The gateway still enforces b's own rule: the expression of b is untouched.
	if got := expr(t, byName(r.Objects)["b"]); !strings.Contains(got, `"eng"`) || strings.Contains(got, `"ops"`) {
		t.Fatalf("b's rule was changed: %s", got)
	}
}

func TestPrimaryOpenToEveryoneWarnsWhenFallbackIsNot(t *testing.T) {
	a := withFallback(model("a", "model-a"), "b")
	rules := []v1.AccessRule{rule("r1", nil, true, "a"), rule("r2", []string{"eng"}, false, "b")}
	r := compileModels([]v1.Model{a, model("b", "model-b")}, rules, route)
	if !hasNote(r, "a", "b is not open to every signed-in person") {
		t.Fatalf("got %v", r.Notes["a"])
	}
}

func TestRetryPolicyAttemptsFollowTheLongestChain(t *testing.T) {
	cases := []struct {
		name   string
		models []v1.Model
		want   int64
	}{
		{"two targets", []v1.Model{withFallback(model("a", "model-a"), "b"), model("b", "model-b")}, 2},
		{"three targets", []v1.Model{withFallback(model("a", "model-a"), "b", "c"), model("b", "model-b"), model("c", "model-c")}, 4},
		{"endpoints count as targets", []v1.Model{withEndpoints(model("a", "a"), "http://x/v1", "http://y/v1", "http://z/v1")}, 6},
		{"capped", []v1.Model{withEndpoints(model("a", "a"), "http://1/v1", "http://2/v1", "http://3/v1", "http://4/v1", "http://5/v1", "http://6/v1", "http://7/v1")}, 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := RetryPolicy(compileModels(c.models, nil, route).Objects, "ambit-system", "ambit-llm")
			if p == nil {
				t.Fatal("no policy")
			}
			got, _, _ := unstructured.NestedInt64(p.Object, "spec", "traffic", "retry", "attempts")
			if got != c.want {
				t.Fatalf("attempts %d, want %d", got, c.want)
			}
		})
	}
}

func TestRetryPolicyShape(t *testing.T) {
	ms := []v1.Model{withFallback(model("a", "model-a"), "b"), model("b", "model-b"), withEndpoints(model("gpt-x", "gpt-x"), "http://x/v1")}
	p := RetryPolicy(compileModels(ms, nil, route).Objects, "ambit-system", "ambit-llm")
	if p.GetName() != RetryPolicyName || p.GetNamespace() != "ambit-system" || p.GetKind() != "AgentgatewayPolicy" {
		t.Fatalf("identity: %s %s %s", p.GetKind(), p.GetNamespace(), p.GetName())
	}
	ref, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	if ref[0].(map[string]any)["name"] != "ambit-llm" || ref[0].(map[string]any)["kind"] != "HTTPRoute" {
		t.Fatalf("targetRefs %v", ref)
	}
	retry, _, _ := unstructured.NestedMap(p.Object, "spec", "traffic", "retry")
	if retry["backoff"] != "10ms" {
		t.Fatalf("backoff %v", retry["backoff"])
	}
	var codes []int64
	for _, c := range retry["codes"].([]any) {
		codes = append(codes, c.(int64))
	}
	if want := []int64{429, 500, 502, 503, 504, 529}; !slices.Equal(codes, want) {
		t.Fatalf("codes %v, want %v", codes, want)
	}
	// Only the wire names that have a failover list retry; llmRequest is not available in a precondition.
	if want := `json(request.body).model in ["gpt-x", "model-a"]`; retry["precondition"] != want {
		t.Fatalf("precondition %q, want %q", retry["precondition"], want)
	}
}

func TestRefuseUnpricedAlsoClosesTheExtraEndpoints(t *testing.T) {
	m := withEndpoints(model("gpt-x", "gpt-x"), "http://x/v1")
	res := compileModels([]v1.Model{m}, []v1.AccessRule{rule("all", nil, true, "gpt-x")}, route)
	refused := RefuseUnpriced(&res, []v1.Model{m}, map[string]v1.ModelPrice{}, []string{"eng"})
	if gs := refused["gpt-x"]; !slices.Equal(gs, []string{"eng"}) {
		t.Fatalf("refused %v, want gpt-x refused to eng (once)", refused)
	}
	for _, n := range []string{"gpt-x", "gpt-x-ep1"} {
		if got := expr(t, byName(res.Objects)[n]); !strings.Contains(got, `"eng"`) {
			t.Fatalf("%s is still open to eng: %s", n, got)
		}
	}
	if _, has := refused["gpt-x-ep1"]; has {
		t.Fatalf("refusals are keyed by Model name, got %v", refused)
	}
}

func TestRulesVersionIgnoresTheVirtualModelButSeesEndpointsAndFallbackCopies(t *testing.T) {
	rules := []v1.AccessRule{rule("all", nil, true, "a", "b")}
	plain := RulesVersion(compileModels([]v1.Model{model("a", "model-a"), model("b", "model-b")}, rules, route).Objects)
	fb := RulesVersion(compileModels([]v1.Model{withFallback(model("a", "model-a"), "b"), model("b", "model-b")}, rules, route).Objects)
	if plain == fb {
		t.Fatal("the copy of a fallback Model carries an access decision and must change the version")
	}
	more := RulesVersion(compileModels([]v1.Model{withEndpoints(model("a", "model-a"), "http://x/v1"), model("b", "model-b")}, rules, route).Objects)
	if plain == more {
		t.Fatal("an extra endpoint carries an access rule and must change the version")
	}
}

func TestAuditRelaxationSkipsTheVirtualModel(t *testing.T) {
	m := withEndpoints(model("gpt-x", "gpt-x"), "http://x/v1")
	res := compileModels([]v1.Model{m}, nil, route)
	for _, o := range RelaxForAudit(res, nil, func(*Result) {}) {
		_, hasPolicies, _ := unstructured.NestedMap(o.Object, "spec", "policies")
		if o.GetName() == "gpt-x-failover" && hasPolicies {
			t.Fatal("a virtual model must not get policies")
		}
		if o.GetName() == "gpt-x-ep1" && expr(t, o) != "true" {
			t.Fatalf("ep1 was not relaxed: %s", expr(t, o))
		}
	}
}

func TestAuditFieldsDoNotRepeatTheEndpointExpression(t *testing.T) {
	rules := []v1.AccessRule{rule("eng", []string{"eng"}, false, "gpt-x")}
	one := compileModels([]v1.Model{withEndpoints(model("gpt-x", "gpt-x"))}, rules, route)
	three := compileModels([]v1.Model{withEndpoints(model("gpt-x", "gpt-x"), "http://x/v1", "http://y/v1")}, rules, route)
	f1, err1 := AuditFields(one, 10)
	f3, err3 := AuditFields(three, 10)
	if err1 != nil || err3 != nil {
		t.Fatal(err1, err3)
	}
	if len(f1) != len(f3) || f1[0].(map[string]any)["expression"] != f3[0].(map[string]any)["expression"] {
		t.Fatalf("endpoints changed the audit field:\n%v\n%v", f1, f3)
	}
}

func TestRouterWarnsWhenATargetHasFallbackOrEndpoints(t *testing.T) {
	a := withFallback(model("a", "model-a"), "b")
	ms := []v1.Model{a, model("b", "model-b")}
	res := compileModels(ms, []v1.AccessRule{rule("all", nil, true, "a", "b")}, route)
	routers := []v1.ModelRouter{{ObjectMeta: model("auto", "").ObjectMeta, Spec: v1.ModelRouterSpec{Default: "a"}}}
	conds, _ := CompileRouters(routers, ms, &res, route, nil, ClassifierInput{})
	c := conds["auto"]
	if c.Reason != "FallbackIgnored" || !strings.Contains(c.Message, "a has a fallback or additional endpoints") {
		t.Fatalf("router condition %+v", c)
	}
}
