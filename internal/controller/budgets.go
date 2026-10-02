package controller

import (
	"sort"
	"strconv"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/pricing"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Budgets become one shared rate limit on the LLM route: the gateway sends each request's group
// and user to envoy's ratelimit service, which counts in Valkey, so every replica sees the same count.
// Requests and estimated dollars. Dollars use `unit: Tokens` descriptors whose cost is computed after the
// answer (agentgateway v1.5.0), counted in units of $0.00001 (pricing.UnitsPerDollar).

const (
	RateLimitDomain    = "ambit-llm"
	RateLimitConfigMap = "ambit-ratelimit-config"
	BudgetPolicyName   = "ambit-llm-budgets"
)

// RateLimit is where the ratelimit service runs and what happens when it is down.
type RateLimit struct {
	Service     string // e.g. ambit-ratelimit; empty turns budgets off
	Port        int64
	FailureMode string // FailOpen or FailClosed
}

// member is the group's name for members and "" for everyone else; "" matches no limit, so
// non-members are not counted against this group.
func member(group string) string {
	q := strconv.Quote(group)
	return "jwt.groups.exists(g, g == " + q + ") ? " + q + ` : ""`
}

// EffectiveBudgets returns the Budget that governs each group: the first by name. The reconciler reports
// the others as DuplicateGroup.
func EffectiveBudgets(budgets []v1.Budget) []v1.Budget {
	sorted := append([]v1.Budget(nil), budgets...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Spec.Group < sorted[j].Spec.Group })
	var out []v1.Budget
	seen := map[string]bool{}
	for _, b := range sorted {
		if !seen[b.Spec.Group] {
			seen[b.Spec.Group] = true
			out = append(out, b)
		}
	}
	return out
}

// DollarGroups lists the groups whose effective Budget has a dollar limit, sorted.
func DollarGroups(budgets []v1.Budget) []string {
	var out []string
	for _, b := range EffectiveBudgets(budgets) {
		if b.Spec.GroupDollarsPerDay != "" || b.Spec.UserDollarsPerDay != "" {
			out = append(out, b.Spec.Group)
		}
	}
	return out
}

// costCEL is the cost of one answer in units: input and output tokens times each model's price per token,
// rounded up. A model missing from the maps makes the expression fail, and agentgateway then skips the
// charge; refusing unpriced models (unpriced.go) keeps that from happening for capped groups.
func costCEL(prices map[string]v1.ModelPrice) string {
	names := make([]string, 0, len(prices))
	for n := range prices {
		names = append(names, n)
	}
	sort.Strings(names)
	in, out := make([]string, 0, len(names)), make([]string, 0, len(names))
	for _, n := range names {
		pi, _ := pricing.PerTokenUnits(prices[n].InputPerMillionTokens) // CRD pattern guarantees a decimal
		po, _ := pricing.PerTokenUnits(prices[n].OutputPerMillionTokens)
		// CEL reads "1" as an int, and int * double fails: write every price as a double literal.
		if !strings.Contains(pi, ".") {
			pi += ".0"
		}
		if !strings.Contains(po, ".") {
			po += ".0"
		}
		q := strconv.Quote(n)
		in = append(in, q+": "+pi)
		out = append(out, q+": "+po)
	}
	return "uint(math.ceil(double(llm.inputTokens) * {" + strings.Join(in, ", ") + "}[llm.requestModel] + double(llm.outputTokens) * {" +
		strings.Join(out, ", ") + "}[llm.requestModel]))"
}

// compileBudgets returns the ratelimit service's config (YAML) and the gateway policy, or a nil
// policy when there are no budgets.
func compileBudgets(budgets []v1.Budget, prices map[string]v1.ModelPrice, ns string, rl RateLimit, llmRoute string) (string, *unstructured.Unstructured) {
	var cfgDescriptors, gwDescriptors []any
	cost := costCEL(prices)
	for _, b := range EffectiveBudgets(budgets) {
		g := b.Spec.Group
		if b.Spec.GroupRequestsPerDay != nil || b.Spec.UserRequestsPerDay != nil {
			d := map[string]any{"key": "group", "value": g}
			if n := b.Spec.GroupRequestsPerDay; n != nil {
				d["rate_limit"] = map[string]any{"unit": "day", "requests_per_unit": *n}
				gwDescriptors = append(gwDescriptors, map[string]any{"unit": "Requests",
					"entries": []any{map[string]any{"name": "group", "expression": member(g)}}})
			}
			if n := b.Spec.UserRequestsPerDay; n != nil {
				d["descriptors"] = []any{map[string]any{"key": "user", "rate_limit": map[string]any{"unit": "day", "requests_per_unit": *n}}}
				gwDescriptors = append(gwDescriptors, map[string]any{"unit": "Requests", "entries": []any{
					map[string]any{"name": "group", "expression": member(g)},
					map[string]any{"name": "user", "expression": "jwt.email"}}})
			}
			cfgDescriptors = append(cfgDescriptors, d)
		}
		if b.Spec.GroupDollarsPerDay != "" || b.Spec.UserDollarsPerDay != "" {
			d := map[string]any{"key": "spend_group", "value": g}
			if u, err := pricing.Units(b.Spec.GroupDollarsPerDay); err == nil {
				d["rate_limit"] = map[string]any{"unit": "day", "requests_per_unit": u}
				gwDescriptors = append(gwDescriptors, map[string]any{"unit": "Tokens", "cost": cost,
					"entries": []any{map[string]any{"name": "spend_group", "expression": member(g)}}})
			}
			if u, err := pricing.Units(b.Spec.UserDollarsPerDay); err == nil {
				d["descriptors"] = []any{map[string]any{"key": "spend_user", "rate_limit": map[string]any{"unit": "day", "requests_per_unit": u}}}
				gwDescriptors = append(gwDescriptors, map[string]any{"unit": "Tokens", "cost": cost, "entries": []any{
					map[string]any{"name": "spend_group", "expression": member(g)},
					map[string]any{"name": "spend_user", "expression": "jwt.email"}}})
			}
			cfgDescriptors = append(cfgDescriptors, d)
		}
	}
	cfg := map[string]any{"domain": RateLimitDomain, "descriptors": cfgDescriptors}
	if cfgDescriptors == nil {
		cfg["descriptors"] = []any{}
	}
	out, _ := yaml.Marshal(cfg)
	if len(gwDescriptors) == 0 {
		return string(out), nil
	}
	p := generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", BudgetPolicyName, ns, map[string]any{
		"targetRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": llmRoute}},
		"traffic": map[string]any{"rateLimit": map[string]any{"global": map[string]any{
			"domain":      RateLimitDomain,
			"backendRef":  map[string]any{"name": rl.Service, "port": rl.Port},
			"failureMode": rl.FailureMode,
			"descriptors": gwDescriptors,
		}}},
	})
	return string(out), p
}

func rateLimitConfigMap(ns, config string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"config.yaml": config}}}
	o.SetAPIVersion("v1")
	o.SetKind("ConfigMap")
	o.SetName(RateLimitConfigMap)
	o.SetNamespace(ns)
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "ambit"})
	return o
}
