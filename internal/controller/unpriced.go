package controller

import (
	"strconv"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// RefuseUnpriced closes the gap a dollar budget cannot count: a model without a price (by pricing.ByModel)
// is refused to members of every group with a dollar budget. Per-model authorization is used because
// route-level authorization runs before the gateway reads the model. It returns the groups refused per Model.
func RefuseUnpriced(res *Result, models []v1.Model, prices map[string]v1.ModelPrice, dollarGroups []string) map[string][]string {
	if len(dollarGroups) == 0 {
		return nil
	}
	deny := unpricedDeny(dollarGroups)
	unpriced := map[string]bool{}
	for _, m := range models {
		if _, ok := prices[m.Spec.Model]; !ok {
			unpriced[m.Name] = true
		}
	}
	refused := map[string][]string{}
	for _, o := range res.Objects {
		if o.GetKind() != "AgentgatewayModel" || !unpriced[modelOf(o)] {
			continue
		}
		path := []string{"spec", "policies", "authorization", "policy", "matchExpressions"}
		exprs, _, _ := unstructured.NestedSlice(o.Object, path...)
		if len(exprs) == 0 {
			continue
		}
		exprs[0] = "(" + exprs[0].(string) + ") && " + deny
		_ = unstructured.SetNestedSlice(o.Object, exprs, path...)
		refused[modelOf(o)] = append([]string(nil), dollarGroups...) // a Model's extra endpoints share its entry
	}
	return refused
}

// unpricedDeny refuses members of the groups.
func unpricedDeny(groups []string) string {
	q := make([]string, len(groups))
	for i, g := range groups {
		q[i] = strconv.Quote(g)
	}
	return "!" + principalGroups + ".exists(g, g in [" + strings.Join(q, ", ") + "])"
}

func unpricedMessage(groups []string) string {
	return "refused for groups " + strings.Join(groups, ", ") + ": no price (they have a dollar budget)"
}

// refuseUnpricedIfEnforced applies RefuseUnpriced only while budgets are on: with limits.enabled=false no
// dollar cap is enforced, so nothing needs protecting.
func refuseUnpricedIfEnforced(rl RateLimit, res *Result, models []v1.Model, prices map[string]v1.ModelPrice, budgets []v1.Budget) map[string][]string {
	if rl.Service == "" {
		return nil
	}
	return RefuseUnpriced(res, models, prices, DollarGroups(budgets))
}
