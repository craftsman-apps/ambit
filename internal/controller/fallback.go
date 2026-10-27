package controller

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

// Fallback and load sharing (Model.spec.additionalEndpoints and Model.spec.fallback), on agentgateway v1.6.0.
//
// A Model that has either field becomes several AgentgatewayModels with one wire name: the Model's own object (now
// Internal), one Internal object per additional endpoint, and one public virtual model with a failover list. Targets
// of one priority share requests by health and speed; the next priority answers when every target at the first is
// failing. Each concrete object carries a health block, and one route-wide retry policy hides the first failure.
// What v1.6.0 cannot do is in docs/routing.md and the pull request that added this.

const (
	// FallbackLabel marks the virtual failover model and the extra-endpoint objects of a Model (value: the Model).
	FallbackLabel = "ambitproject.dev/fallback"
	// RetryPolicyName is the generated route-wide retry policy.
	RetryPolicyName = "ambit-llm-retry"

	// unhealthyCondition lists 429 on purpose: agentgateway's default is 5xx only, and then a rate-limited provider
	// is never skipped. 529 (overloaded) is covered by >= 500.
	unhealthyCondition = `response.code >= 500 || response.code == 429`
	evictionDuration   = "10s" // grows by this much on every repeated eviction, so it stays short
	retryBackoff       = "10ms"
	maxRetryAttempts   = 8
)

var (
	modelLabel = v1.GroupVersion.Group + "/model"
	// retryCodes are the failures that move a request on; the same set marks a target unhealthy.
	retryCodes = []any{int64(429), int64(500), int64(502), int64(503), int64(504), int64(529)}
)

func failoverName(model string) string        { return model + "-failover" }
func endpointName(model string, k int) string { return fmt.Sprintf("%s-ep%d", model, k) } // k counts from 1

func usesFailover(m v1.Model) bool {
	return len(m.Spec.AdditionalEndpoints) > 0 || len(m.Spec.Fallback) > 0
}

// isFailover is the virtual failover model of a Model with additional endpoints or a fallback.
func isFailover(o *unstructured.Unstructured) bool {
	_, ok, _ := unstructured.NestedMap(o.Object, "spec", "virtualModel", "failover")
	return ok
}

// modelOf is the Model an AgentgatewayModel was generated for (extra endpoints included); its own name otherwise.
func modelOf(o *unstructured.Unstructured) string {
	if n := o.GetLabels()[modelLabel]; n != "" {
		return n
	}
	return o.GetName()
}

// failoverPlan is what Compile found out about one Model that has additional endpoints or a fallback.
type failoverPlan struct {
	on        bool     // false: the fields are ignored (see notes) and the Model compiles as a plain one
	endpoints int      // additional endpoints, when on
	fallback  []string // usable fallback Models, in order
	notes     []string // shown on the Model's Ready condition
}

// planFailover decides, for every Model that uses the fields, which fallbacks are usable and what to tell the admin.
func planFailover(models []v1.Model, access map[string]Access) map[string]*failoverPlan {
	byName := map[string]v1.Model{}
	for _, m := range models {
		byName[m.Name] = m
	}
	plans := map[string]*failoverPlan{}
	for _, m := range models {
		if !usesFailover(m) {
			continue
		}
		p := &failoverPlan{}
		plans[m.Name] = p
		if strings.Contains(m.Spec.Model, "*") {
			p.notes = append(p.notes, "it serves a wildcard ("+m.Spec.Model+"); additional endpoints and fallback need one exact model name and are ignored")
			continue
		}
		if clash := nameClash(m, byName); clash != "" {
			p.notes = append(p.notes, "a Model named "+clash+" exists, and Ambit needs that name for this Model's failover objects; additional endpoints and fallback are ignored")
			continue
		}
		for _, f := range m.Spec.Fallback {
			fm, ok := byName[f]
			switch {
			case f == m.Name:
				p.notes = append(p.notes, "a Model cannot fall back to itself")
			case !ok:
				p.notes = append(p.notes, "no Model named "+f+"; that fallback is dropped, the rest is applied")
			case strings.Contains(fm.Spec.Model, "*"):
				p.notes = append(p.notes, f+" serves a wildcard ("+fm.Spec.Model+"); a fallback must name one model, so it is dropped")
			default:
				p.fallback = append(p.fallback, f)
				if len(fm.Spec.Fallback) > 0 {
					p.notes = append(p.notes, f+" has its own fallback; it is not followed")
				}
				if msg := notCovered(m.Name, f, access[m.Name], access[f]); msg != "" {
					p.notes = append(p.notes, msg)
				}
			}
		}
		if p.on = len(m.Spec.AdditionalEndpoints) > 0 || len(p.fallback) > 0; p.on {
			p.endpoints = len(m.Spec.AdditionalEndpoints)
		}
	}
	return plans
}

// nameClash returns the name of a Model that equals one of the object names generated for m, or "".
func nameClash(m v1.Model, byName map[string]v1.Model) string {
	names := []string{failoverName(m.Name)}
	for k := range m.Spec.AdditionalEndpoints {
		names = append(names, endpointName(m.Name, k+1))
	}
	for _, n := range names {
		if _, ok := byName[n]; ok {
			return n
		}
	}
	return ""
}

// notCovered says who may use the Model but not its fallback. The gateway checks the fallback's own rule for every
// request, so those people get 403 when the Model fails; this only tells the admin.
func notCovered(model, fallback string, m, f Access) string {
	switch {
	case f.AllSignedIn, !m.AllSignedIn && len(m.Groups) == 0 && len(m.Users) == 0:
		return ""
	case m.AllSignedIn:
		return fallback + " is not open to every signed-in person, who may use " + model + "; if " + model + " fails they get 403 instead of " + fallback + "'s answer"
	}
	var missing []string
	for _, g := range m.Groups {
		if !slices.Contains(f.Groups, g) {
			missing = append(missing, g)
		}
	}
	for _, u := range m.Users {
		if !slices.Contains(f.Users, u) {
			missing = append(missing, u)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fallback + " is not open to " + strings.Join(missing, ", ") + " (who may use " + model + "); if " + model + " fails they get 403 instead of " + fallback + "'s answer"
}

// modelObjects is the AgentgatewayModels of one Model: just its own object unless it has failover or is a fallback
// of another Model (then its objects carry the health block).
func modelObjects(m v1.Model, a Access, route RouteRef, plans map[string]*failoverPlan, targeted bool) []*unstructured.Unstructured {
	primary := agentgatewayModel(m, a, route)
	p := plans[m.Name]
	on := p != nil && p.on
	if !on && !targeted {
		return []*unstructured.Unstructured{primary}
	}
	objs := []*unstructured.Unstructured{primary}
	if on {
		for k, e := range m.Spec.AdditionalEndpoints {
			ep := agentgatewayModel(endpointModel(m, e), a, route)
			ep.SetName(endpointName(m.Name, k+1))
			ep.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "ambit", modelLabel: m.Name, FallbackLabel: m.Name})
			objs = append(objs, ep)
		}
	}
	for _, o := range objs {
		if on {
			_ = unstructured.SetNestedField(o.Object, "Internal", "spec", "visibility")
		}
		_ = unstructured.SetNestedField(o.Object, healthPolicy(), "spec", "policies", "health")
	}
	if on {
		objs = append(objs, failoverModel(m, p, plans, route))
	}
	return objs
}

func healthPolicy() map[string]any {
	return map[string]any{
		"unhealthyCondition": unhealthyCondition,
		"eviction":           map[string]any{"consecutiveFailures": int64(1), "duration": evictionDuration},
	}
}

// endpointModel is m with the settings of one additional endpoint, which replace the endpoint settings as a whole.
func endpointModel(m v1.Model, e v1.ModelEndpoint) v1.Model {
	c := *m.DeepCopy()
	c.Spec.BaseURL, c.Spec.CredentialsRef = e.BaseURL, e.CredentialsRef
	c.Spec.Azure, c.Spec.Bedrock, c.Spec.VertexAI = e.Azure, e.Bedrock, e.VertexAI
	c.Spec.AdditionalEndpoints, c.Spec.Fallback = nil, nil
	return c
}

// failoverModel is the public virtual model: all endpoints of m at priority 0, then the endpoints of each fallback
// Model, one priority per fallback.
func failoverModel(m v1.Model, p *failoverPlan, plans map[string]*failoverPlan, route RouteRef) *unstructured.Unstructured {
	var targets []any
	add := func(name string, priority int64) {
		targets = append(targets, map[string]any{"modelRef": map[string]any{"name": name}, "priority": priority})
	}
	group := func(model string, priority int64) {
		add(model, priority)
		if q := plans[model]; q != nil && q.on {
			for k := 1; k <= q.endpoints; k++ {
				add(endpointName(model, k), priority)
			}
		}
	}
	group(m.Name, 0)
	for i, f := range p.fallback {
		group(f, int64(i+1))
	}
	o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"parentRefs":   []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": route.Name}},
		"match":        map[string]any{"model": m.Spec.Model},
		"virtualModel": map[string]any{"failover": map[string]any{"targets": targets}},
	}}}
	o.SetAPIVersion("agentgateway.dev/v1alpha1")
	o.SetKind("AgentgatewayModel")
	o.SetName(failoverName(m.Name))
	o.SetNamespace(m.Namespace)
	o.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "ambit", FallbackLabel: m.Name})
	o.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: v1.GroupVersion.String(), Kind: "Model", Name: m.Name, UID: m.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}})
	return o
}

// RetryPolicy is the route-wide retry policy for the Models that have failover, or nil when none has. A retry first
// goes back to the same target, so a chain of T targets needs 2 x (T - 1) attempts (measured on v1.6.0). It is
// limited to those Models' wire names by a precondition: llmRequest is not available there, the request body is.
func RetryPolicy(objs []*unstructured.Unstructured, ns, route string) *unstructured.Unstructured {
	var wires []string
	longest := 0
	for _, o := range objs {
		if o.GetKind() != "AgentgatewayModel" || !isFailover(o) {
			continue
		}
		ts, _, _ := unstructured.NestedSlice(o.Object, "spec", "virtualModel", "failover", "targets")
		longest = max(longest, len(ts))
		wire, _, _ := unstructured.NestedString(o.Object, "spec", "match", "model")
		wires = append(wires, strconv.Quote(wire))
	}
	if len(wires) == 0 {
		return nil
	}
	slices.Sort(wires)
	attempts := min(max(2*(longest-1), 2), maxRetryAttempts)
	return generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", RetryPolicyName, ns, map[string]any{
		"targetRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": route}},
		"traffic": map[string]any{"retry": map[string]any{
			"attempts":     int64(attempts),
			"backoff":      retryBackoff,
			"codes":        slices.Clone(retryCodes),
			"precondition": "json(request.body).model in [" + strings.Join(wires, ", ") + "]",
		}},
	})
}
