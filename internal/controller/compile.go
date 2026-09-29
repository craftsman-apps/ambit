// Package controller turns Ambit objects (Model, AccessRule) into agentgateway objects.
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
	Objects        []*unstructured.Unstructured // AgentgatewayModel per Model, sorted by name
	Access         map[string]Access            // by Model name
	RuleConditions map[string]metav1.Condition  // "Ready" condition by AccessRule name
}

// Compile is a pure function: the same inputs in any order give the same output.
// A model that no rule names is denied to everyone.
func Compile(models []v1.Model, rules []v1.AccessRule, route RouteRef) Result {
	known := map[string]bool{}
	for _, m := range models {
		known[m.Name] = true
	}
	res := Result{Access: map[string]Access{}, RuleConditions: map[string]metav1.Condition{}}
	groups := map[string]map[string]bool{}
	for _, r := range rules {
		var missing []string
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
		res.RuleConditions[r.Name] = ruleCondition(r, missing)
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
	return res
}

func ruleCondition(r v1.AccessRule, missing []string) metav1.Condition {
	c := metav1.Condition{Type: "Ready", ObservedGeneration: r.Generation}
	if len(missing) > 0 {
		c.Status, c.Reason = metav1.ConditionFalse, "UnknownModel"
		c.Message = fmt.Sprintf("no Model named %s in this namespace; the rest of the rule is applied", strings.Join(missing, ", "))
		return c
	}
	c.Status, c.Reason, c.Message = metav1.ConditionTrue, "Compiled", "applied to every listed model"
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
