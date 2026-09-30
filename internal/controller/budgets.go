package controller

import (
	"sort"
	"strconv"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Budgets become one shared rate limit on the LLM route: the gateway sends each request's group
// and user to envoy's ratelimit service, which counts in Valkey, so every replica sees the same count.
// Requests, not tokens: agentgateway v1.5.0 cannot count tokens for model routes (build sheet risk 13).

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

// compileBudgets returns the ratelimit service's config (YAML) and the gateway policy, or a nil
// policy when there are no budgets.
func compileBudgets(budgets []v1.Budget, ns string, rl RateLimit, llmRoute string) (string, *unstructured.Unstructured) {
	sorted := append([]v1.Budget(nil), budgets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Spec.Group < sorted[j].Spec.Group })
	var cfgDescriptors, gwDescriptors []any
	seen := map[string]bool{}
	// Budgets for one group: the first by name wins (the reconciler reports the others).
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Spec.Group < sorted[j].Spec.Group })
	for _, b := range sorted {
		if seen[b.Spec.Group] {
			continue
		}
		seen[b.Spec.Group] = true
		d := map[string]any{"key": "group", "value": b.Spec.Group}
		if n := b.Spec.GroupRequestsPerDay; n != nil {
			d["rate_limit"] = map[string]any{"unit": "day", "requests_per_unit": *n}
			gwDescriptors = append(gwDescriptors, map[string]any{"unit": "Requests",
				"entries": []any{map[string]any{"name": "group", "expression": member(b.Spec.Group)}}})
		}
		if n := b.Spec.UserRequestsPerDay; n != nil {
			d["descriptors"] = []any{map[string]any{"key": "user", "rate_limit": map[string]any{"unit": "day", "requests_per_unit": *n}}}
			gwDescriptors = append(gwDescriptors, map[string]any{"unit": "Requests", "entries": []any{
				map[string]any{"name": "group", "expression": member(b.Spec.Group)},
				map[string]any{"name": "user", "expression": "jwt.email"}}})
		}
		cfgDescriptors = append(cfgDescriptors, d)
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
