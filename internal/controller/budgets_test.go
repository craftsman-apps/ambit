package controller

import (
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
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
      requests_per_unit: 50
      unit: day
  key: group
  rate_limit:
    requests_per_unit: 500
    unit: day
  value: eng
- key: group
  rate_limit:
    requests_per_unit: 1000
    unit: day
  value: ml-research
domain: ambit-llm
`
	if cfg != want {
		t.Fatalf("ratelimit config:\n%s\nwant\n%s", cfg, want)
	}
	d, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "rateLimit", "global", "descriptors")
	if len(d) != 3 {
		t.Fatalf("gateway descriptors %v", d)
	}
	first := d[0].(map[string]any)["entries"].([]any)[0].(map[string]any)["expression"]
	if first != `jwt.groups.exists(g, g == "eng") ? "eng" : ""` {
		t.Fatalf("member expression %v", first)
	}
	if fm, _, _ := unstructured.NestedString(p.Object, "spec", "traffic", "rateLimit", "global", "failureMode"); fm != "FailOpen" {
		t.Fatalf("failureMode %s", fm)
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
	want := `uint(math.ceil(double(llm.inputTokens) * {"gpt-4.1": 0.015, "mock/echo": 0.1}[llm.requestModel] + double(llm.outputTokens) * {"gpt-4.1": 0.06, "mock/echo": 0.4}[llm.requestModel]))`
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
	if user[0].(map[string]any)["name"] != "spend_group" || user[1].(map[string]any)["name"] != "spend_user" || user[1].(map[string]any)["expression"] != "jwt.email" {
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
	if strings.Join(units, ",") != "Requests,Tokens" {
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
