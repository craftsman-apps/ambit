package controller

import (
	"github.com/craftsman-apps/ambit/internal/identity"
	"reflect"
	"strconv"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

var rl = RateLimit{Service: "ambit-ratelimit", Port: 8081, FailureMode: "FailOpen"}

func budget(name, group string, groupN, userN *int64) v1.Budget {
	return v1.Budget{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.BudgetSpec{Group: group, GroupRequestsPerDay: groupN, UserRequestsPerDay: userN}}
}

func TestBudgetsCompileToSharedLimits(t *testing.T) {
	cfg, p := compileBudgets([]v1.Budget{
		budget("ml", "ml-research", ptr.To[int64](1000), nil),
		budget("eng", "eng", ptr.To[int64](500), ptr.To[int64](50)),
	}, nil, "ambit-system", rl, "ambit-llm")
	want := `descriptors:
- descriptors:
  - key: user
    rate_limit:
      requests_per_unit: 49
      unit: day
  key: group
  rate_limit:
    requests_per_unit: 499
    unit: day
  value: eng
- key: group
  rate_limit:
    requests_per_unit: 999
    unit: day
  value: ml-research
domain: ambit-llm
`
	if cfg != want {
		t.Fatalf("ratelimit config:\n%s\nwant\n%s", cfg, want)
	}
	d, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "rateLimit", "global", "descriptors")
	if len(d) != 6 { // per limit: one counted after the answer, one for embeddings
		t.Fatalf("gateway descriptors %v", d)
	}
	first := d[1].(map[string]any)["entries"].([]any)[0].(map[string]any)["expression"]
	if first != identity.PrincipalGroups+`.exists(g, g == "eng") ? "eng" : ""` {
		t.Fatalf("member expression %v", first)
	}
	if fm, _, _ := unstructured.NestedString(p.Object, "spec", "traffic", "rateLimit", "global", "failureMode"); fm != "FailOpen" {
		t.Fatalf("failureMode %s", fm)
	}
}

// Only requests the gateway let through count against a request limit. agentgateway checks a "Requests"
// descriptor (and adds 1) with the route policies, before the model's own authorization, so a request refused
// with 403 counted too (found 2026-10-13: 3 answers and 1 refusal showed "used 4"). A "Tokens" descriptor is
// checked after the model is chosen and authorized, and charged its cost (here 1) after the answer.
func TestRequestLimitsCountOnlyAnsweredRequests(t *testing.T) {
	cfg, p := compileBudgets([]v1.Budget{budget("eng", "eng", ptr.To[int64](5), ptr.To[int64](3))}, nil, "ambit-system", rl, "ambit-llm")
	ds, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "rateLimit", "global", "descriptors")
	if len(ds) != 4 {
		t.Fatalf("descriptors %v", ds)
	}
	for i, d := range ds {
		m := d.(map[string]any)
		if i%2 == 0 && (m["unit"] != "Tokens" || m["cost"] != "1") {
			t.Fatalf("descriptor %v: want unit Tokens, cost 1", m)
		}
		if i%2 == 1 && (m["unit"] != "Requests" || !strings.HasPrefix(m["limitOverride"].(string), embeddingsPath)) {
			t.Fatalf("descriptor %v: want the embeddings descriptor", m)
		}
	}
	// The check before a request adds nothing and refuses only when the count is above the configured number,
	// so the configured number is one less than the Budget's: 5 a day allows answers 1 to 5 and refuses the 6th.
	if !strings.Contains(cfg, "requests_per_unit: 4\n") || !strings.Contains(cfg, "requests_per_unit: 2\n") {
		t.Fatalf("config:\n%s", cfg)
	}
}

func TestNoBudgetsMeansNoRateLimitCalls(t *testing.T) {
	cfg, p := compileBudgets(nil, nil, "ambit-system", rl, "ambit-llm")
	if p != nil || !strings.Contains(cfg, "descriptors: []") {
		t.Fatalf("policy %v cfg %s", p, cfg)
	}
}

func dollars(name, group, groupD, userD string) v1.Budget {
	return v1.Budget{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.BudgetSpec{Group: group, GroupDollarsPerDay: groupD, UserDollarsPerDay: userD}}
}

var testPrices = map[string]v1.ModelPrice{
	"mock/echo": {InputPerMillionTokens: "1.00", OutputPerMillionTokens: "4.00"},
	"gpt-4.1":   {InputPerMillionTokens: "0.15", OutputPerMillionTokens: "0.6"},
}

func TestCostCEL(t *testing.T) {
	want := `(uint(llm.inputTokens) * {"gpt-4.1": 150000u, "mock/echo": 1000000u}[llm.requestModel] + uint(llm.outputTokens) * {"gpt-4.1": 600000u, "mock/echo": 4000000u}[llm.requestModel] + 9999999u) / 10000000u`
	if got := costCEL(testPrices); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestDollarOnlyBudget(t *testing.T) {
	cfg, p := compileBudgets([]v1.Budget{dollars("ml", "ml-research", "0.00005", "5")}, testPrices, "ambit-system", rl, "ambit-llm")
	want := `descriptors:
- descriptors:
  - key: spend_user
    rate_limit:
      requests_per_unit: 500000
      unit: day
  key: spend_group
  rate_limit:
    requests_per_unit: 5
    unit: day
  value: ml-research
domain: ambit-llm
`
	if cfg != want {
		t.Fatalf("config:\n%s", cfg)
	}
	if p == nil {
		t.Fatal("no policy for a dollar-only budget")
	}
	ds, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "rateLimit", "global", "descriptors")
	if len(ds) != 2 {
		t.Fatalf("descriptors %v", ds)
	}
	for _, d := range ds {
		m := d.(map[string]any)
		if m["unit"] != "Tokens" || m["cost"] != costCEL(testPrices) {
			t.Fatalf("descriptor %v", m)
		}
	}
	user := ds[1].(map[string]any)["entries"].([]any)
	if user[0].(map[string]any)["name"] != "spend_group" || user[1].(map[string]any)["name"] != "spend_user" || user[1].(map[string]any)["expression"] != spendUser() {
		t.Fatalf("user entries %v", user)
	}
}

func TestRequestAndDollarLimitsInOneBudget(t *testing.T) {
	cfg, p := compileBudgets([]v1.Budget{{ObjectMeta: metav1.ObjectMeta{Name: "eng"}, Spec: v1.BudgetSpec{Group: "eng",
		UserRequestsPerDay: ptr.To[int64](2), GroupDollarsPerDay: "50"}}}, testPrices, "ambit-system", rl, "ambit-llm")
	if !strings.Contains(cfg, "key: group") || !strings.Contains(cfg, "key: spend_group") || !strings.Contains(cfg, "requests_per_unit: 5000000") {
		t.Fatalf("config:\n%s", cfg)
	}
	ds, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "rateLimit", "global", "descriptors")
	units := []string{}
	for _, d := range ds {
		units = append(units, d.(map[string]any)["unit"].(string))
	}
	if strings.Join(units, ",") != "Tokens,Requests,Tokens" {
		t.Fatalf("units %v", units)
	}
}

// A person in two capped groups matches both group descriptors (each by membership), so both are charged.
func TestTwoCappedGroupsAreChargedSeparately(t *testing.T) {
	_, p := compileBudgets([]v1.Budget{dollars("a", "eng", "10", ""), dollars("b", "ml-research", "10", "")}, testPrices, "ambit-system", rl, "ambit-llm")
	ds, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "rateLimit", "global", "descriptors")
	if len(ds) != 2 {
		t.Fatalf("%v", ds)
	}
	for i, g := range []string{"eng", "ml-research"} {
		e := ds[i].(map[string]any)["entries"].([]any)[0].(map[string]any)
		if e["expression"] != member(g) {
			t.Fatalf("%d: %v", i, e)
		}
	}
}

func TestEffectiveBudgetsFirstByName(t *testing.T) {
	got := EffectiveBudgets([]v1.Budget{dollars("z", "eng", "1", ""), dollars("a", "eng", "2", ""), dollars("m", "ml", "3", "")})
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "m" {
		t.Fatalf("%v", got)
	}
	if g := DollarGroups([]v1.Budget{dollars("a", "eng", "2", ""), budget("r", "ops", ptr.To[int64](5), nil)}); strings.Join(g, ",") != "eng" {
		t.Fatalf("dollar groups %v", g)
	}
}

// A token without an email claim is still charged per person (by its subject); a failing expression would
// make agentgateway skip both the check and the charge.
func TestSpendUserFallsBackToSubject(t *testing.T) {
	old := RequireEmail
	defer func() { RequireEmail = old }()
	RequireEmail = false
	if spendUser() != identity.Principal(false) || !strings.Contains(spendUser(), "jwt.sub") {
		t.Fatal(spendUser())
	}
}

// A streamed chat request that turns usage reporting off would have no token counts and cost nothing. For
// members of a group with a dollar limit the gateway turns it back on (the gateway itself does so only
// when stream_options is absent).
// The rewrite that turns usage reporting on for dollar-capped groups is a backend policy: agentgateway applies it
// after a ModelRouter has picked the model. In v1.5.0 a route-level (traffic) rewrite was undone by the router,
// which rebuilt the body from its original copy (found 2026-10-06: routed streams cost 0; fixed upstream in v1.6.0,
// #3287); the backend policy works on both.
func TestDollarBudgetForcesUsageInStreams(t *testing.T) {
	budgets := []v1.Budget{
		{ObjectMeta: metav1.ObjectMeta{Name: "ml"}, Spec: v1.BudgetSpec{Group: "ml-research", GroupDollarsPerDay: "50"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "eng"}, Spec: v1.BudgetSpec{Group: "eng", UserRequestsPerDay: ptr.To[int64](2)}},
	}
	_, p := compileBudgets(budgets, testPrices, "ambit-system", rl, "ambit-llm")
	if _, found, _ := unstructured.NestedFieldNoCopy(p.Object, "spec", "traffic", "transformation"); found {
		t.Fatal("the route policy still rewrites requests; a ModelRouter undoes that")
	}
	u := usagePolicy(budgets, "ambit-system", "ambit-llm")
	if u == nil || u.GetName() != UsagePolicyName {
		t.Fatalf("usage policy %v", u)
	}
	if ref, _, _ := unstructured.NestedSlice(u.Object, "spec", "targetRefs"); len(ref) != 1 || ref[0].(map[string]any)["name"] != "ambit-llm" {
		t.Fatalf("targetRefs %v", ref)
	}
	tr, _, _ := unstructured.NestedSlice(u.Object, "spec", "backend", "ai", "transformations")
	if len(tr) != 1 || tr[0].(map[string]any)["field"] != "stream_options" || tr[0].(map[string]any)["expression"] != usageField([]string{"ml-research"}) {
		t.Fatalf("transformations %v", tr)
	}
	e := usageField([]string{"ml-research"})
	if !strings.Contains(e, `g in ["ml-research"]`) || strings.Contains(e, `"eng"`) || !strings.Contains(e, `{"include_usage": true}`) {
		t.Fatalf("expression %s", e)
	}
	// No route list: agentgateway v1.6.0 keeps its default API types under a backend AI policy (upstream #3391),
	// and a copied list would override the defaults of the version that runs (v1.6.0 changed them).
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "backend", "ai", "routes"); found {
		t.Fatal("the usage policy sets its own route types")
	}
	// request limits only: nothing to charge, no rewrite
	if usagePolicy(budgets[1:], "ambit-system", "ambit-llm") != nil {
		t.Fatal("a request-only budget rewrites requests")
	}
}

// A Budget of N admits exactly N answers a day, the smallest Budget (1) included: the first request passes, the
// second is refused. envoy ratelimit (base_limiter.go) refuses when count + hits > limit; the check before a request
// sends 0 hits, and each answer adds 1 afterwards.
func TestRequestLimitAdmitsExactlyTheBudget(t *testing.T) {
	for _, n := range []int64{1, 2, 5} {
		cfg, _ := compileBudgets([]v1.Budget{budget("eng", "eng", nil, ptr.To(n))}, nil, "ambit-system", rl, "ambit-llm")
		var limit int64 = -1
		for _, line := range strings.Split(cfg, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "requests_per_unit: "); ok {
				limit, _ = strconv.ParseInt(v, 10, 64)
			}
		}
		if limit < 0 {
			t.Fatalf("Budget %d: no limit in config:\n%s", n, cfg)
		}
		count, answered := int64(0), int64(0)
		for range n + 3 {
			if count+0 > limit { // the check before the request
				continue
			}
			answered++
			count++ // the charge after the answer
		}
		if answered != n {
			t.Fatalf("Budget %d (configured %d): %d answers, want %d", n, limit, answered, n)
		}
	}
}

// Embeddings count against request limits when they arrive (owner decision 2026-10-13): agentgateway v1.6.0 has
// no step after an embeddings answer that could add the count. A "Requests" descriptor for /v1/embeddings shares the
// counter with the after-answer ones, with the Budget's own number as its limit (limitOverride), and the
// after-answer descriptors skip embeddings. A refused embeddings request also counts (documented).
func TestEmbeddingsCountWhenTheyArrive(t *testing.T) {
	const n = 3
	_, p := compileBudgets([]v1.Budget{budget("eng", "eng", nil, ptr.To[int64](n))}, nil, "ambit-system", rl, "ambit-llm")
	ds, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "rateLimit", "global", "descriptors")
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("request", cel.DynType), cel.Variable("jwt", cel.DynType), cel.Variable("apiKey", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	eval := func(expr, path string, groups []any) (ref.Val, error) {
		t.Helper()
		ast, iss := env.Parse(expr) // agentgateway's CEL does not type-check
		if iss.Err() != nil {
			t.Fatalf("does not parse: %v\n%s", iss.Err(), expr)
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		v, _, err := prg.Eval(map[string]any{"request": map[string]any{"path": path},
			"jwt": map[string]any{"sub": "s1", "email": "alice@acme.test", "groups": groups}, "apiKey": map[string]any{}})
		return v, err
	}
	// descriptorApplies: every entry evaluates and the limit override (if any) evaluates to a limit; agentgateway
	// skips the descriptor otherwise. It returns the limit override's number, or -1 without one.
	descriptorApplies := func(d map[string]any, path string, groups []any) (bool, int64) {
		for _, e := range d["entries"].([]any) {
			if _, err := eval(e.(map[string]any)["expression"].(string), path, groups); err != nil {
				return false, 0
			}
		}
		lo, ok := d["limitOverride"].(string)
		if !ok {
			return true, -1
		}
		v, err := eval(lo, path, groups)
		if err != nil {
			return false, 0
		}
		m, err := v.ConvertToNative(reflect.TypeOf(map[string]any{}))
		if err != nil {
			return false, 0
		}
		lim, ok := m.(map[string]any)["requestsPerUnit"].(int64)
		if !ok || m.(map[string]any)["unit"] != "day" {
			t.Fatalf("limit override %v", m)
		}
		return true, lim
	}
	var requests, tokens []map[string]any
	for _, d := range ds {
		m := d.(map[string]any)
		if m["unit"] == "Requests" {
			requests = append(requests, m)
		} else {
			tokens = append(tokens, m)
		}
	}
	if len(requests) != 1 || len(tokens) != 1 {
		t.Fatalf("want one Requests (embeddings) and one Tokens descriptor: %v", ds)
	}
	member, other := []any{"eng"}, []any{"sales"}
	if ok, lim := descriptorApplies(requests[0], "/v1/embeddings", member); !ok || lim != n {
		t.Fatalf("embeddings by a member: applies %v, limit %d (want the Budget's %d)", ok, lim, n)
	}
	for _, c := range []struct {
		path   string
		groups []any
	}{{"/v1/chat/completions", member}, {"/v1/embeddings", other}, {"/v1/chat/completions", other}} {
		// A limit override applies whatever the entries' values, so it must not apply to anyone else.
		if ok, _ := descriptorApplies(requests[0], c.path, c.groups); ok {
			t.Fatalf("the embeddings descriptor applies to %s for %v", c.path, c.groups)
		}
	}
	if ok, _ := descriptorApplies(tokens[0], "/v1/embeddings", member); ok {
		t.Fatal("the after-answer descriptor applies to embeddings: it would see the count already raised")
	}
	if ok, _ := descriptorApplies(tokens[0], "/v1/chat/completions", member); !ok {
		t.Fatal("the after-answer descriptor does not apply to chat")
	}
	// A day of chat and embeddings: exactly n are let through, and every embeddings request counts.
	cfgLimit := int64(n - 1)
	for _, seq := range [][]string{{"e", "e", "e", "e"}, {"c", "e", "c", "e", "c"}, {"e", "c", "c", "c", "e"}} {
		count, through := int64(0), 0
		for _, k := range seq {
			if k == "e" {
				count++ // ratelimit adds the hit, then compares
				if count <= n {
					through++
				}
				continue
			}
			if count > cfgLimit { // after-answer descriptor: check with 0 hits
				continue
			}
			through++
			count++
		}
		if through != n {
			t.Fatalf("%v: %d let through, want %d", seq, through, n)
		}
	}
}
