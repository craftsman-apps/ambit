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
	}, "ambit-system", rl, "ambit-llm")
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
	cfg, p := compileBudgets(nil, "ambit-system", rl, "ambit-llm")
	if p != nil || !strings.Contains(cfg, "descriptors: []") {
		t.Fatalf("policy %v cfg %s", p, cfg)
	}
}
