package controller

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/routerconfig"
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

// ClassifierInput is what CompileRouters needs to know about the classifier side.
type ClassifierInput struct {
	Installed bool            // the ambit-router Deployment exists in this install
	HasKey    map[string]bool // classifier Model object name -> its credentials Secret has an Authorization entry
}

// CompileRouters adds one virtual AgentgatewayModel per valid router to res.Objects and returns each router's
// Ready condition, plus the configuration ambit-router needs for the routers of type LLM. Call it after
// RefuseUnpriced: each line repeats its target's final access expression. refused is RefuseUnpriced's result: the
// groups each unpriced Model is refused to (nil when budgets are off).
func CompileRouters(routers []v1.ModelRouter, models []v1.Model, res *Result, route RouteRef, refused map[string][]string, ci ClassifierInput) (map[string]metav1.Condition, routerconfig.Config) {
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

	// Pass 1: validate; collect the configuration of the valid LLM routers. Lines need its hash, so they come after.
	type compiled struct {
		r             v1.ModelRouter
		wire          string
		kept          []v1.RouterRoute
		reasons, msgs []string // warnings: the router is served
		classified    bool     // its category routes have lines
	}
	var valid []compiled
	var cfg routerconfig.Config
	wireTaken := map[string]string{}  // wire name -> router that holds it
	categories := map[string]string{} // category name -> description, first declared (by router name)
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
		typ := r.Spec.Classifier.Type
		routes := r.Spec.Routes
		if typ != "Rules" && typ != "LLM" {
			routes = nil // type None: always the default
		}
		if n := keywordCount(routes); len(routes) > 16 || n > maxKeywords {
			conds[r.Name] = cond(metav1.ConditionFalse, "TooManyRoutes", fmt.Sprintf("%d routes and %d keywords; at most 16 and %d", len(routes), n, maxKeywords))
			continue
		}
		c := compiled{r: r, wire: wire, classified: typ == "LLM" && ci.Installed}
		var cfgRouter *routerconfig.Router
		if c.classified {
			var msg string
			cfgRouter, msg = llmConfig(r, wire, byObject, ci, res.Access[r.Name])
			if msg != "" {
				conds[r.Name] = cond(metav1.ConditionFalse, "ClassifierModelInvalid", msg)
				continue
			}
			if msg := categoryConflict(r, categories); msg != "" {
				conds[r.Name] = cond(metav1.ConditionFalse, "CategoryConflict", msg)
				continue
			}
		}
		declared := map[string]bool{}
		for _, cat := range r.Spec.Classifier.Categories {
			declared[cat.Name] = true
		}
		var missing, undeclared []string
		needsClassifier := false
		for _, rt := range routes {
			if rt.Category != "" {
				if !c.classified { // no classifier: the whole route is ignored
					needsClassifier = true
					continue
				}
				if !declared[rt.Category] {
					undeclared = append(undeclared, rt.Category)
					continue
				}
			}
			if _, ok := byObject[rt.Model]; !ok {
				missing = append(missing, rt.Model)
				continue
			}
			c.kept = append(c.kept, rt)
		}
		if amb := ambiguous(append(targetNames(c.kept), r.Spec.Default), byObject, models); amb != "" {
			conds[r.Name] = cond(metav1.ConditionFalse, "AmbiguousTarget", amb)
			continue
		}
		if len(missing) > 0 {
			c.reasons, c.msgs = append(c.reasons, "UnknownModel"), append(c.msgs, "no Model named "+strings.Join(missing, ", ")+"; those routes are dropped, the rest is applied")
		}
		if len(undeclared) > 0 {
			c.reasons, c.msgs = append(c.reasons, "UnknownCategory"), append(c.msgs, "categories not declared in classifier.categories: "+strings.Join(undeclared, ", ")+"; those routes are dropped, the rest is applied")
		}
		switch {
		case typ == "LLM" && !ci.Installed:
			c.reasons, c.msgs = append(c.reasons, "ClassifierNotInstalled"), append(c.msgs, "ambit-router is not installed (routing.router.enabled=false); routes with a category are ignored, the rest is applied")
		case needsClassifier:
			c.reasons, c.msgs = append(c.reasons, "CategoryNeedsClassifier"), append(c.msgs, "routes with a category need a classifier (type LLM); they are ignored, the rest is applied")
		}
		if !mayShare(res.Access[r.Name], res.Access[r.Spec.Default], refused[r.Spec.Default]) {
			c.reasons, c.msgs = append(c.reasons, "DefaultNotAllowed"), append(c.msgs, "no group may use both this router and its default "+r.Spec.Default+"; its users get 400 whenever no route they may use matches")
		}
		if cfgRouter != nil {
			for _, cat := range r.Spec.Classifier.Categories {
				categories[cat.Name] = cat.Description
			}
			cfg.Routers = append(cfg.Routers, *cfgRouter)
		}
		valid = append(valid, c)
	}
	if len(cfg.Routers) > 0 { // routers are already in name order
		cfg.Hash = routerconfig.HashOf(cfg.Routers)
	}

	// Pass 2: lines, in route order, then the default.
	for _, c := range valid {
		access := CEL(res.Access[c.r.Name])
		var lines []any
		for _, rt := range c.kept {
			lines = append(lines, map[string]any{
				"when":     "(" + access + ") && " + routedPath + " && (" + routeCondition(rt, cfg.Hash) + ") && (" + final[rt.Model] + ")",
				"modelRef": map[string]any{"name": rt.Model},
			})
		}
		lines = append(lines, map[string]any{
			"when":     "(" + access + ") && (" + final[c.r.Spec.Default] + ")",
			"modelRef": map[string]any{"name": c.r.Spec.Default},
		})
		res.Objects = append(res.Objects, virtualModel(c.r, c.wire, lines, route))
		// Warnings: the router is served; the first sets the reason and the message names every one.
		cnd := cond(metav1.ConditionTrue, "Compiled", fmt.Sprintf("%d routes and the default, served as %q", len(c.kept), c.wire))
		if len(c.reasons) > 0 {
			cnd = cond(metav1.ConditionFalse, c.reasons[0], strings.Join(c.msgs, "; "))
		}
		conds[c.r.Name] = cnd
	}
	return conds, cfg
}

// llmConfig is a type-LLM router's entry in ambit-router's configuration, or why its classifier cannot be called.
func llmConfig(r v1.ModelRouter, wire string, byObject map[string]v1.Model, ci ClassifierInput, a Access) (*routerconfig.Router, string) {
	cl := r.Spec.Classifier
	if cl.LLM == nil {
		return nil, "type LLM needs classifier.llm.model"
	}
	m, ok := byObject[cl.LLM.Model]
	if !ok {
		return nil, "no Model named " + cl.LLM.Model + " for the classifier"
	}
	endpoint, ok := LLMEndpoint(m)
	if !ok {
		return nil, "classifier Model " + m.Name + " has no OpenAI-compatible endpoint: set baseURL, or use provider OpenAI or Openrouter"
	}
	if len(m.Spec.Formats) > 0 && !slices.Contains(m.Spec.Formats, "Completions") {
		return nil, "classifier Model " + m.Name + " does not list the Completions format; ambit-router calls /chat/completions"
	}
	if !ci.HasKey[m.Name] {
		return nil, "the Secret " + m.Spec.CredentialsRef.Name + " of classifier Model " + m.Name + " is missing or has no Authorization entry"
	}
	out := &routerconfig.Router{Name: r.Name, Wire: wire, Type: "LLM", Groups: a.Groups, AllSignedIn: a.AllSignedIn,
		LLM:           &routerconfig.LLM{Endpoint: endpoint, Model: m.Spec.Model, CredentialKey: m.Name},
		TimeoutMillis: cl.TimeoutOrDefault().Milliseconds(), MaxBytes: cl.MaxBytesOrDefault(), RatePerMinute: cl.RateOrDefault()}
	for _, c := range cl.Categories {
		out.Categories = append(out.Categories, routerconfig.Category{Name: c.Name, Description: c.Description})
	}
	return out, ""
}

// categoryConflict: a category name means the same in every router (spec, CategoryConflict).
func categoryConflict(r v1.ModelRouter, declared map[string]string) string {
	for _, c := range r.Spec.Classifier.Categories {
		if d, ok := declared[c.Name]; ok && d != c.Description {
			return "category " + c.Name + " is declared by another router with a different description; a category name must mean the same everywhere"
		}
	}
	return ""
}

// LLMEndpoint is the OpenAI-compatible base URL the classifier calls.
func LLMEndpoint(m v1.Model) (string, bool) {
	switch {
	case m.Spec.BaseURL != "":
		return strings.TrimSuffix(m.Spec.BaseURL, "/"), true
	case m.Spec.Provider == "OpenAI":
		return "https://api.openai.com/v1", true
	case m.Spec.Provider == "Openrouter":
		return "https://openrouter.ai/api/v1", true
	}
	return "", false
}

func nameTaken(r v1.ModelRouter, wire string, models []v1.Model, taken map[string]string, byObject map[string]v1.Model) string {
	if other, ok := taken[wire]; ok {
		return "ModelRouter " + other + " already serves " + wire
	}
	if _, ok := byObject[r.Name]; ok { // AccessRules name objects: one grant would open both
		return "a Model is also named " + r.Name + ", so AccessRules could not tell them apart; rename one"
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

// routeCondition binds the text once ([text].exists(t, ...)); keywords are lowercased and Go-quoted (valid CEL). A
// category is read from ambit-router's label, and only under the configuration hash these lines were built with.
func routeCondition(rt v1.RouterRoute, hash string) string {
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
	var out []string
	if rt.Category != "" {
		out = append(out, categoryCondition(hash, rt.Category))
	}
	if len(parts) > 0 {
		// size() also works on a list, so a multi-part message must not reach the length conditions.
		out = append(out, "["+lastUserText+"].exists(t, type(t) == string && "+strings.Join(parts, " && ")+")")
	}
	return strings.Join(out, " && ")
}

// categoryCondition: never has(request.headers[...]) — invalid CEL, which agentgateway accepts and treats as false
// (spec fact 12). A missing header is an evaluation error, which counts as false.
func categoryCondition(hash, name string) string {
	return `request.headers["x-ambit-config"] == ` + strconv.Quote(hash) + ` && request.headers["x-ambit-route"] == ` + strconv.Quote(name)
}

// mayShare reports whether some signed-in user may use both a and b.
// mayShare reports whether some signed-in user may use both a and b, where members of the groups in refusedB
// are refused b (an unpriced default for dollar-capped groups).
func mayShare(a, b Access, refusedB []string) bool {
	open := func(gs []string) []string {
		var out []string
		for _, g := range gs {
			if !slices.Contains(refusedB, g) {
				out = append(out, g)
			}
		}
		return out
	}
	aGroups, bGroups := open(a.Groups), open(b.Groups)
	switch {
	case a.AllSignedIn && b.AllSignedIn:
		return true
	case a.AllSignedIn:
		return len(bGroups) > 0
	case b.AllSignedIn:
		return len(aGroups) > 0
	}
	for _, g := range aGroups {
		if slices.Contains(bGroups, g) {
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
