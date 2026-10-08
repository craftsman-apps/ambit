package controller

import (
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/pricing"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var pricingByModel = pricing.ByModel

func TestRefuseUnpriced(t *testing.T) {
	m := func(name, model string, p *v1.ModelPrice) v1.Model {
		return v1.Model{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.ModelSpec{Model: model, Provider: "OpenAI",
			CredentialsRef: &v1.SecretRef{Name: "ambit-llm-x"}, Price: p}}
	}
	priced := &v1.ModelPrice{InputPerMillionTokens: "1", OutputPerMillionTokens: "4"}
	models := []v1.Model{m("echo", "mock/echo", priced), m("free", "free", &v1.ModelPrice{InputPerMillionTokens: "0", OutputPerMillionTokens: "0"}),
		m("raw", "raw", nil), m("dup-a", "dup", priced), m("dup-b", "dup", nil)}
	rules := []v1.AccessRule{{ObjectMeta: metav1.ObjectMeta{Name: "r"}, Spec: v1.AccessRuleSpec{Groups: []string{"ml-research", "eng"},
		Models: []string{"echo", "free", "raw", "dup-a", "dup-b"}}}}
	res := Compile(models, nil, rules, RouteRef{Name: "ambit-llm"})
	refused := RefuseUnpriced(&res, models, pricingByModel(models), []string{"eng", "ml-research"})
	if strings.Join(refused["raw"], ",") != "eng,ml-research" || len(refused["dup-a"]) != 2 || len(refused["dup-b"]) != 2 || refused["echo"] != nil || refused["free"] != nil {
		t.Fatalf("refused %v", refused)
	}
	expr := func(name string) string {
		for _, o := range res.Objects {
			if o.GetKind() == "AgentgatewayModel" && o.GetName() == name {
				e, _, _ := unstructured.NestedSlice(o.Object, "spec", "policies", "authorization", "policy", "matchExpressions")
				return e[0].(string)
			}
		}
		return ""
	}
	if got := expr("raw"); got != `(jwt.groups.exists(g, g in ["eng", "ml-research"])) && !jwt.groups.exists(g, g in ["eng", "ml-research"])` {
		t.Fatalf("raw: %s", got)
	}
	if got := expr("echo"); strings.Contains(got, "!jwt") {
		t.Fatalf("echo restricted: %s", got)
	}
	if RefuseUnpriced(&res, models, pricingByModel(models), nil) != nil {
		t.Fatal("no dollar groups must change nothing")
	}
	if msg := unpricedMessage([]string{"eng", "ml-research"}); msg != "refused for groups eng, ml-research: no price (they have a dollar budget)" {
		t.Fatal(msg)
	}
}

// With budgets off (no ratelimit service) no dollar cap is enforced, so nothing is refused for lack of a price.
func TestUnpricedNotRefusedWhenBudgetsAreOff(t *testing.T) {
	models := []v1.Model{{ObjectMeta: metav1.ObjectMeta{Name: "raw"}, Spec: v1.ModelSpec{Model: "raw", Provider: "OpenAI", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-x"}}}}
	rules := []v1.AccessRule{{ObjectMeta: metav1.ObjectMeta{Name: "r"}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"raw"}}}}
	budgets := []v1.Budget{{ObjectMeta: metav1.ObjectMeta{Name: "eng"}, Spec: v1.BudgetSpec{Group: "eng", GroupDollarsPerDay: "5"}}}
	res := Compile(models, nil, rules, RouteRef{Name: "ambit-llm"})
	if got := refuseUnpricedIfEnforced(RateLimit{}, &res, models, pricingByModel(models), budgets); got != nil {
		t.Fatalf("refused with budgets off: %v", got)
	}
	if got := refuseUnpricedIfEnforced(rl, &res, models, pricingByModel(models), budgets); len(got["raw"]) != 1 {
		t.Fatalf("not refused with budgets on: %v", got)
	}
}
