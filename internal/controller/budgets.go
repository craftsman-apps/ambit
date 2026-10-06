package controller

import (
	"fmt"
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

// spendUser names the person a dollar cap charges: the email, or the subject when the token has no email
// claim (a failing expression would make agentgateway skip both the check and the charge).
const spendUser = "has(jwt.email) ? jwt.email : jwt.sub"

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

// costCEL is the cost of one answer in units: input and output tokens times each model's price, rounded
// up. Integer arithmetic keeps it exact (in floating point, 2 x 0.1 + 7 x 0.4 rounded up to 4 units, not 3):
// prices are in micros (pricing.Micros), and units = (tokens x micros) / 10^7, rounded up. A model missing
// from the maps makes the expression fail, and agentgateway then skips the charge; refusing unpriced
// models (unpriced.go) keeps that from happening for capped groups.
func costCEL(prices map[string]v1.ModelPrice) string {
	names := make([]string, 0, len(prices))
	for n := range prices {
		names = append(names, n)
	}
	sort.Strings(names)
	in, out := make([]string, 0, len(names)), make([]string, 0, len(names))
	for _, n := range names {
		pi, _ := pricing.Micros(prices[n].InputPerMillionTokens) // CRD pattern guarantees at most 6 decimals
		po, _ := pricing.Micros(prices[n].OutputPerMillionTokens)
		q := strconv.Quote(n)
		in = append(in, fmt.Sprintf("%s: %du", q, pi))
		out = append(out, fmt.Sprintf("%s: %du", q, po))
	}
	return "(uint(llm.inputTokens) * {" + strings.Join(in, ", ") + "}[llm.requestModel] + uint(llm.outputTokens) * {" +
		strings.Join(out, ", ") + "}[llm.requestModel] + 9999999u) / 10000000u"
}

// UsagePolicyName is the generated policy that turns usage reporting on for dollar-capped groups.
const UsagePolicyName = "ambit-llm-usage"

// usageField is the expression for stream_options: for a streamed chat request from a member of a dollar-capped
// group it sets include_usage to true (other stream options are kept); otherwise it keeps the field as sent (an
// expression that fails because the field is absent leaves it unset). Without token counts the answer would cost
// nothing. It reads llmRequest, the body the provider will get.
func usageField(groups []string) string {
	q := make([]string, len(groups))
	for i, g := range groups {
		q[i] = strconv.Quote(g)
	}
	return `request.path == "/v1/chat/completions" && llmRequest.stream == true && jwt.groups.exists(g, g in [` + strings.Join(q, ", ") + `]) ? ` +
		`(has(llmRequest.stream_options) ? llmRequest.stream_options.merge({"include_usage": true}) : {"include_usage": true}) : ` +
		`llmRequest.stream_options`
}

// defaultRouteTypes is agentgateway v1.5.0's default_route_types (crates/agentgateway/src/llm/model_router.rs).
// A backend AI policy without routes drops those defaults and every request is parsed as chat (embeddings were
// answered as chat, 2026-10-06), so the usage policy repeats them. Recheck on every agentgateway upgrade.
var defaultRouteTypes = map[string]any{
	"/v1/chat/completions": "Completions", "/v1/messages": "Messages", "/v1/messages/count_tokens": "AnthropicTokenCount",
	":rawPredict": "Messages", ":streamRawPredict": "Messages", ":generateContent": "GenerateContent",
	":streamGenerateContent": "GenerateContent", ":countTokens": "GeminiCountTokens", "/v1/responses": "Responses",
	"/v1/images/generations": "Detect", "/v1/images/edits": "Detect", "/v1/images/variations": "Detect",
	"/v1/responses/compact": "Detect", "/v1/embeddings": "Embeddings", "/v1/rerank": "Rerank", "/v2/rerank": "Rerank",
	"*": "Passthrough",
}

// usagePolicy is a backend policy on the LLM route, so agentgateway applies it when it calls the chosen model:
// after a ModelRouter has picked it. A route-level (traffic) rewrite was undone there, because the router rebuilds
// the body from its original copy (agentgateway v1.5.0; found 2026-10-06: routed streams cost nothing). Nil
// when no group has a dollar limit.
func usagePolicy(budgets []v1.Budget, ns, llmRoute string) *unstructured.Unstructured {
	groups := DollarGroups(budgets)
	if len(groups) == 0 {
		return nil
	}
	return generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", UsagePolicyName, ns, map[string]any{
		"targetRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": llmRoute}},
		"backend": map[string]any{"ai": map[string]any{
			"routes":          defaultRouteTypes,
			"transformations": []any{map[string]any{"field": "stream_options", "expression": usageField(groups)}},
		}},
	})
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
					map[string]any{"name": "spend_user", "expression": spendUser}}})
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
	traffic := map[string]any{"rateLimit": map[string]any{"global": map[string]any{
		"domain":      RateLimitDomain,
		"backendRef":  map[string]any{"name": rl.Service, "port": rl.Port},
		"failureMode": rl.FailureMode,
		"descriptors": gwDescriptors,
	}}}
	p := generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", BudgetPolicyName, ns, map[string]any{
		"targetRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": llmRoute}},
		"traffic":    traffic,
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
