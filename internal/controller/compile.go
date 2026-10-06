// Package controller turns Ambit objects (Model, Server, AccessRule) into agentgateway and ToolHive objects.
package controller

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

// RouteRef is the HTTPRoute the chart creates for LLM traffic; every model attaches to it.
type RouteRef struct{ Name string }

// Access is who may use one model, after merging every AccessRule that names it.
type Access struct {
	AllSignedIn bool
	Groups      []string
}

type Result struct {
	// AgentgatewayModel per Model, then MCPServer per Server, then the shared MCP backend and its
	// access policy (only when a Server exists); each group sorted by name.
	Objects        []*unstructured.Unstructured
	Access         map[string]Access           // by Model name
	RuleConditions map[string]metav1.Condition // "Ready" condition by AccessRule name
}

// Compile is a pure function: the same inputs in any order give the same output.
// A model, server or tool that no rule names is denied to everyone.
// Router names (ModelRouter objects) count as known: rules grant them like Models, but no object is generated
// for them here (see CompileRouters).
func Compile(models []v1.Model, servers []v1.Server, rules []v1.AccessRule, route RouteRef, routers ...string) Result {
	known := map[string]bool{}
	for _, m := range models {
		known[m.Name] = true
	}
	for _, name := range routers {
		known[name] = true
	}
	knownServer := map[string]bool{}
	for _, s := range servers {
		knownServer[s.Name] = true
	}
	res := Result{Access: map[string]Access{}, RuleConditions: map[string]metav1.Condition{}}
	groups := map[string]map[string]bool{}
	for _, r := range rules {
		var missing, missingServers []string
		for _, sa := range r.Spec.Servers {
			if !knownServer[sa.Name] {
				missingServers = append(missingServers, sa.Name)
			}
		}
		for _, name := range r.Spec.Models {
			if !known[name] {
				missing = append(missing, name)
				continue
			}
			a := res.Access[name]
			a.AllSignedIn = a.AllSignedIn || r.Spec.AllSignedIn
			res.Access[name] = a
			if groups[name] == nil {
				groups[name] = map[string]bool{}
			}
			for _, g := range r.Spec.Groups {
				groups[name][g] = true
			}
		}
		res.RuleConditions[r.Name] = ruleCondition(r, missing, missingServers)
	}
	for name, set := range groups {
		a := res.Access[name]
		for g := range set {
			a.Groups = append(a.Groups, g)
		}
		sort.Strings(a.Groups)
		res.Access[name] = a
	}
	sorted := slices.Clone(models)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, m := range sorted {
		res.Objects = append(res.Objects, agentgatewayModel(m, res.Access[m.Name], route))
	}
	res.Objects = append(res.Objects, compileServers(servers, rules)...)
	return res
}

func ruleCondition(r v1.AccessRule, missing, missingServers []string) metav1.Condition {
	c := metav1.Condition{Type: "Ready", ObservedGeneration: r.Generation}
	var msgs []string
	if len(missing) > 0 {
		c.Reason = "UnknownModel"
		msgs = append(msgs, "no Model named "+strings.Join(missing, ", "))
	}
	if len(missingServers) > 0 {
		c.Reason = "UnknownServer"
		msgs = append(msgs, "no Server named "+strings.Join(missingServers, ", "))
	}
	if len(msgs) > 0 {
		if len(msgs) == 2 {
			c.Reason = "UnknownModelOrServer"
		}
		c.Status = metav1.ConditionFalse
		c.Message = fmt.Sprintf("%s in this namespace; the rest of the rule is applied", strings.Join(msgs, "; "))
		return c
	}
	c.Status, c.Reason, c.Message = metav1.ConditionTrue, "Compiled", "applied to every listed model and server"
	return c
}

// CEL returns the authorization expression for one model.
func CEL(a Access) string {
	switch {
	case a.AllSignedIn:
		return "true"
	case len(a.Groups) == 0:
		return "false"
	}
	q := make([]string, len(a.Groups))
	for i, g := range a.Groups {
		q[i] = strconv.Quote(g) // group names are restricted by the CRD pattern, so Go quoting is valid CEL
	}
	return "jwt.groups.exists(g, g in [" + strings.Join(q, ", ") + "])"
}

func agentgatewayModel(m v1.Model, a Access, route RouteRef) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"parentRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": route.Name}},
			"provider":   m.Spec.Provider,
			"match":      map[string]any{"model": m.Spec.Model},
			"policies": map[string]any{
				"auth": map[string]any{"secretRef": map[string]any{"name": m.Spec.CredentialsRef.Name}},
				"authorization": map[string]any{
					"action": "Allow",
					"policy": map[string]any{"matchExpressions": []any{CEL(a)}},
				},
			},
		},
	}}
	if m.Spec.BaseURL != "" {
		unstructured.SetNestedField(o.Object, m.Spec.BaseURL, "spec", "baseURL")
	}
	o.SetAPIVersion("agentgateway.dev/v1alpha1")
	o.SetKind("AgentgatewayModel")
	o.SetName(m.Name)
	o.SetNamespace(m.Namespace)
	// Only Ambit's own labels: copying Argo CD's tracking metadata would make Argo CD prune these (GitOps rule 1).
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "ambit", v1.GroupVersion.Group + "/model": m.Name})
	o.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: v1.GroupVersion.String(), Kind: "Model", Name: m.Name, UID: m.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}})
	return o
}

// WithoutArgumentLimits removes every server grant that carries argument limits, for clusters without
// ambit-guard (protection off), where nothing would enforce them: such a grant must grant nothing rather
// than everything. It returns the servers removed, by rule name.
func WithoutArgumentLimits(rules []v1.AccessRule) ([]v1.AccessRule, map[string][]string) {
	dropped := map[string][]string{}
	out := make([]v1.AccessRule, 0, len(rules))
	for _, r := range rules {
		var keep []v1.ServerAccess
		for _, sa := range r.Spec.Servers {
			if len(sa.Arguments) > 0 {
				dropped[r.Name] = append(dropped[r.Name], sa.Name)
				continue
			}
			keep = append(keep, sa)
		}
		r = *r.DeepCopy()
		r.Spec.Servers = keep
		out = append(out, r)
	}
	return out, dropped
}

// ApplyDropped marks the rules whose limited grants were removed.
func ApplyDropped(res *Result, dropped map[string][]string) {
	for name, servers := range dropped {
		c := res.RuleConditions[name]
		c.Status, c.Reason = metav1.ConditionFalse, "ArgumentLimitsNeedProtection"
		c.Message = "argument limits on " + strings.Join(servers, ", ") + " need protection (ambit-guard) to be enforced, so those grants are not applied; the rest of the rule is"
		res.RuleConditions[name] = c
	}
}
