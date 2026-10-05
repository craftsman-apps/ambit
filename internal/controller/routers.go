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

// RouterLabel marks the virtual AgentgatewayModel generated for a ModelRouter.
const RouterLabel = "ambitproject.dev/router"

const maxKeywords = 32

// routedPath: only chat completions are routed (v1); other paths reach the default line only.
const routedPath = `request.path == "/v1/chat/completions"`

// lastUserText is the last user message when its content is a string. When the virtual model's lines are
// evaluated the body is not parsed yet, so it is read from the raw request (llmRequest). A list content or no
// user message makes it fail, and a failing line counts as false.
const lastUserText = `llmRequest.messages.filter(m, m.role == "user")[size(llmRequest.messages.filter(m, m.role == "user")) - 1].content`

func routerObjectName(name string) string { return "router-" + name }

// CompileRouters adds one virtual AgentgatewayModel per valid router to res.Objects and returns each router's
// Ready condition. Call it after RefuseUnpriced: each line repeats its target's final access expression.
func CompileRouters(routers []v1.ModelRouter, models []v1.Model, res *Result, route RouteRef) map[string]metav1.Condition {
	conds := map[string]metav1.Condition{}
	byObject := map[string]v1.Model{}
	for _, m := range models {
		byObject[m.Name] = m
	}
	final := map[string]string{} // Model object name -> its generated authorization expression
	for _, o := range res.Objects {
		if o.GetKind() == "AgentgatewayModel" {
			e, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "policies", "authorization", "policy", "matchExpressions")
			if len(e) == 1 {
				final[o.GetName()] = e[0]
			}
		}
	}
	sorted := append([]v1.ModelRouter(nil), routers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	wireTaken := map[string]string{} // wire name -> router that holds it
	for _, r := range sorted {
		wire := r.WireName()
		if msg := nameTaken(r, wire, models, wireTaken, byObject); msg != "" {
			conds[r.Name] = cond(metav1.ConditionFalse, "NameTaken", msg)
			continue
		}
		wireTaken[wire] = r.Name
		if _, ok := byObject[r.Spec.Default]; !ok {
			conds[r.Name] = cond(metav1.ConditionFalse, "UnknownDefault", "no Model named "+r.Spec.Default+" for the default")
			continue
		}
		routes := r.Spec.Routes
		if r.Spec.Classifier.Type != "Rules" {
			routes = nil // type None: always the default
		}
		if n := keywordCount(routes); len(routes) > 16 || n > maxKeywords {
			conds[r.Name] = cond(metav1.ConditionFalse, "TooManyRoutes", fmt.Sprintf("%d routes and %d keywords; at most 16 and %d", len(routes), n, maxKeywords))
			continue
		}
		var missing []string
		var kept []v1.RouterRoute
		needsClassifier := false
		for _, rt := range routes {
			if rt.Category != "" { // no classifier in types None and Rules: the whole route is ignored
				needsClassifier = true
				continue
			}
			if _, ok := byObject[rt.Model]; !ok {
				missing = append(missing, rt.Model)
				continue
			}
			kept = append(kept, rt)
		}
		if amb := ambiguous(append(targetNames(kept), r.Spec.Default), byObject, models); amb != "" {
			conds[r.Name] = cond(metav1.ConditionFalse, "AmbiguousTarget", amb)
			continue
		}
		access := CEL(res.Access[r.Name])
		var lines []any
		for _, rt := range kept {
			lines = append(lines, map[string]any{
				"when":     "(" + access + ") && " + routedPath + " && (" + routeCondition(rt) + ") && (" + final[rt.Model] + ")",
				"modelRef": map[string]any{"name": rt.Model},
			})
		}
		lines = append(lines, map[string]any{
			"when":     "(" + access + ") && (" + final[r.Spec.Default] + ")",
			"modelRef": map[string]any{"name": r.Spec.Default},
		})
		res.Objects = append(res.Objects, virtualModel(r, wire, lines, route))
		c := cond(metav1.ConditionTrue, "Compiled", fmt.Sprintf("%d routes and the default, served as %q", len(kept), wire))
		switch {
		case len(missing) > 0:
			c = cond(metav1.ConditionFalse, "UnknownModel", "no Model named "+strings.Join(missing, ", ")+"; those routes are dropped, the rest is applied")
		case needsClassifier:
			c = cond(metav1.ConditionFalse, "CategoryNeedsClassifier", "routes with a category need a classifier (type VSR or LLM); they are ignored, the rest is applied")
		case !mayShare(res.Access[r.Name], res.Access[r.Spec.Default]):
			c = cond(metav1.ConditionFalse, "DefaultNotAllowed", "no group may use both this router and its default "+r.Spec.Default+"; its users get 400 whenever no route they may use matches")
		}
		conds[r.Name] = c
	}
	return conds
}

func nameTaken(r v1.ModelRouter, wire string, models []v1.Model, taken map[string]string, byObject map[string]v1.Model) string {
	if other, ok := taken[wire]; ok {
		return "ModelRouter " + other + " already serves " + wire
	}
	if _, ok := byObject[routerObjectName(r.Name)]; ok {
		return "a Model is named " + routerObjectName(r.Name) + ", the name of this router's generated object; rename one"
	}
	for _, m := range models {
		if wireMatches(m.Spec.Model, wire) {
			return "Model " + m.Name + " already serves " + wire
		}
	}
	return ""
}

// wireMatches follows agentgateway's match.model: exact, "*", a suffix wildcard "gpt-*" or a prefix wildcard "*-latest".
func wireMatches(pattern, name string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	case strings.HasPrefix(pattern, "*"):
		return strings.HasSuffix(name, strings.TrimPrefix(pattern, "*"))
	}
	return pattern == name
}

// ambiguous reports a target that agentgateway could resolve to another Model: a wildcard target (it needs a
// concrete name), or a wire name that another Model also serves, exactly or by a wildcard.
func ambiguous(targets []string, byObject map[string]v1.Model, models []v1.Model) string {
	for _, t := range targets {
		wire := byObject[t].Spec.Model
		if strings.Contains(wire, "*") {
			return "Model " + t + " serves a wildcard (" + wire + "); a router target must name one model"
		}
		for _, m := range models {
			if m.Name != t && wireMatches(m.Spec.Model, wire) {
				return wire + " is also served by Model " + m.Name + "; a router target must be unique"
			}
		}
	}
	return ""
}

func targetNames(routes []v1.RouterRoute) []string {
	var out []string
	for _, r := range routes {
		out = append(out, r.Model)
	}
	return out
}

func keywordCount(routes []v1.RouterRoute) int {
	n := 0
	for _, r := range routes {
		n += len(r.Keywords)
	}
	return n
}

// routeCondition binds the text once ([text].exists(t, ...)); keywords are lowercased and Go-quoted (valid CEL).
func routeCondition(rt v1.RouterRoute) string {
	var parts []string
	if len(rt.Keywords) > 0 {
		var kw []string
		for _, k := range rt.Keywords {
			kw = append(kw, "l.contains("+strconv.Quote(strings.ToLower(k))+")")
		}
		parts = append(parts, "[t.lowerAscii()].exists(l, "+strings.Join(kw, " || ")+")")
	}
	if rt.MinBytes != nil {
		parts = append(parts, fmt.Sprintf("size(t) >= %d", *rt.MinBytes))
	}
	if rt.MaxBytes != nil {
		parts = append(parts, fmt.Sprintf("size(t) <= %d", *rt.MaxBytes))
	}
	// size() also works on a list, so a multi-part message must not reach the length conditions.
	return "[" + lastUserText + "].exists(t, type(t) == string && " + strings.Join(parts, " && ") + ")"
}

// mayShare reports whether some signed-in user may use both a and b.
func mayShare(a, b Access) bool {
	if (a.AllSignedIn && (b.AllSignedIn || len(b.Groups) > 0)) || (b.AllSignedIn && len(a.Groups) > 0) {
		return true
	}
	for _, g := range a.Groups {
		if slices.Contains(b.Groups, g) {
			return true
		}
	}
	return false
}

func virtualModel(r v1.ModelRouter, wire string, lines []any, route RouteRef) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"parentRefs":   []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": route.Name}},
		"match":        map[string]any{"model": wire},
		"virtualModel": map[string]any{"conditional": map[string]any{"targets": lines}},
	}}}
	o.SetAPIVersion("agentgateway.dev/v1alpha1")
	o.SetKind("AgentgatewayModel")
	o.SetName(routerObjectName(r.Name))
	o.SetNamespace(r.Namespace)
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "ambit", RouterLabel: r.Name})
	o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "ModelRouter", Name: r.Name, UID: r.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}})
	return o
}
