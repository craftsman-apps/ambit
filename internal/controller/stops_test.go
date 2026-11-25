package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func stopCM(gen int64, n int) (*corev1.ConfigMap, StopList) {
	l := StopList{Gen: gen}
	for i := 0; i < n; i++ {
		l.Entries = append(l.Entries, StopEntry{Kind: "agent", Name: fmt.Sprintf("agent-%04d", i), Gen: gen, RequestedAt: time.Now()})
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: StopsConfigMap, Namespace: "ns", UID: "u"}}, l
}

// The size plan: at most 150 names per policy, at most 1,000 entries, an empty list is one policy that allows everyone.
func TestStopPolicyChunks(t *testing.T) {
	routes := []string{"ambit-llm", "ambit-mcp", "ambit-mcp-public"}
	for n, want := range map[int]int{0: 1, 1: 1, 150: 1, 151: 2, 300: 2, 301: 3, 1000: 7} {
		cm, l := stopCM(5, n)
		ps, err := stopPolicies(cm, l, routes, "")
		if err != nil || len(ps) != want {
			t.Fatalf("%d entries: %d policies, %v; want %d", n, len(ps), err, want)
		}
		names := 0
		for i, p := range ps {
			if p.GetName() != fmt.Sprintf("ambit-agent-stops-%d", i) || p.GetAnnotations()[StopsGenAnnotation] != "5" {
				t.Errorf("policy %s %v", p.GetName(), p.GetAnnotations())
			}
			e, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
			if len(e) != 1 || len(e[0]) > maxStopExpr {
				t.Errorf("policy %d expression of %d characters", i, len(e[0]))
			}
			names += strings.Count(e[0], `"agent-`)
			if refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs"); len(refs) != 3 {
				t.Errorf("targets %v", refs)
			}
		}
		if names != n {
			t.Errorf("%d entries: %d names in the policies", n, names)
		}
		if n == 0 {
			if e, _, _ := unstructured.NestedStringSlice(ps[0].Object, "spec", "traffic", "authorization", "policy", "matchExpressions"); e[0] != "true" {
				t.Errorf("empty list: %v", e)
			}
		}
	}
	cm, l := stopCM(5, 1001)
	if _, err := stopPolicies(cm, l, routes, ""); err == nil {
		t.Error("1,001 entries were rendered")
	}
	l.Entries = []StopEntry{{Kind: "team", Name: "x"}}
	if _, err := stopPolicies(cm, l, routes, ""); err == nil {
		t.Error("an unknown kind was rendered")
	}
	// grants and agents in the same chunks, with an issuer
	l = StopList{Gen: 2, Entries: []StopEntry{{Kind: "agent", Name: "a"}, {Kind: "grant", Name: "g1"}, {Kind: "grant", Name: "g1"}}}
	ps, _ := stopPolicies(cm, l, routes, "https://auth.example.com")
	e, _, _ := unstructured.NestedStringSlice(ps[0].Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
	if !strings.Contains(e[0], `jwt.ambit_grant in ["g1"]`) || !strings.Contains(e[0], `apiKey.agent in ["a"]`) {
		t.Errorf("expression %s", e[0])
	}
}

func policyWithStatus(gen int64, gens map[string]int64) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{}}
	o.SetGeneration(gen)
	var anc []any
	for ref, observed := range gens {
		kind, name, _ := strings.Cut(ref, "/")
		cond := func(typ string) any {
			return map[string]any{"type": typ, "status": "True", "observedGeneration": observed}
		}
		anc = append(anc, map[string]any{"controllerName": agentgatewayController, "ancestorRef": map[string]any{"kind": kind, "name": name},
			"conditions": []any{cond("Accepted"), cond("Attached")}})
	}
	_ = unstructured.SetNestedSlice(o.Object, anc, "status", "ancestors")
	return o
}

func TestAncestorsAccept(t *testing.T) {
	routes := []string{"ambit-mcp", "ambit-llm"}
	for name, c := range map[string]struct {
		o    *unstructured.Unstructured
		want bool
	}{
		"every route at the generation":    {policyWithStatus(3, map[string]int64{"HTTPRoute/ambit-mcp": 3, "HTTPRoute/ambit-llm": 3}), true},
		"one route behind":                 {policyWithStatus(3, map[string]int64{"HTTPRoute/ambit-mcp": 3, "HTTPRoute/ambit-llm": 2}), false},
		"one route missing":                {policyWithStatus(3, map[string]int64{"HTTPRoute/ambit-mcp": 3}), false},
		"the Gateway itself at generation": {policyWithStatus(3, map[string]int64{"Gateway/ambit": 3}), true},
		"another Gateway does not count":   {policyWithStatus(3, map[string]int64{"Gateway/other": 3}), false},
		"no status":                        {policyWithStatus(3, nil), false},
		"a later generation counts":        {policyWithStatus(3, map[string]int64{"HTTPRoute/ambit-mcp": 4, "HTTPRoute/ambit-llm": 4}), true},
		"another controller's entry":       {&unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"ancestors": []any{map[string]any{"controllerName": "other"}}}}}, false},
	} {
		if got := ancestorsAccept(c.o, "ambit", routes); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if ancestorsAccept(policyWithStatus(1, nil), "ambit", nil) {
		t.Error("no routes: nothing is acknowledged")
	}
}
