package controller

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// The stop reconciler against a real API server: policies per chunk on every route, the acknowledgment per Gateway
// (only when every route of the Gateway has accepted the generation), shrinking, and the fail-closed rules.
func TestStopReconcilerAgainstAPIServer(t *testing.T) {
	needEnv(t)
	ns := "stops"
	namespace(t, ns)
	ctx := t.Context()
	route := func(name, gateway string) {
		o := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": gateway}},
			"rules":      []any{map[string]any{"backendRefs": []any{map[string]any{"name": "x", "port": int64(80)}}}}}}}
		o.SetGroupVersionKind(httpRouteGVK)
		o.SetNamespace(ns)
		o.SetName(name)
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	route("ambit-mcp", "ambit")
	route("ambit-llm", "ambit")
	route("ambit-mcp-public", "ambit-public")
	r := &StopReconciler{Client: envClient, Live: envClient, Namespace: ns, Routes: []string{"ambit-mcp", "ambit-llm"}, Settle: 0}
	list := func(gen int64, n int) {
		t.Helper()
		l := StopList{Gen: gen}
		for i := 0; i < n; i++ {
			l.Entries = append(l.Entries, StopEntry{Kind: "agent", Name: fmt.Sprintf("agent-%04d", i), Gen: gen, Slot: i, RequestedAt: time.Now().Add(-5 * time.Second)})
		}
		b, _ := json.Marshal(l)
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: StopsConfigMap, Namespace: ns}}
		if err := envClient.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
			cm.Data = map[string]string{"list.json": string(b)}
			if err := envClient.Create(ctx, cm); err != nil {
				t.Fatal(err)
			}
			return
		}
		cm.Data = map[string]string{"list.json": string(b)}
		if err := envClient.Update(ctx, cm); err != nil {
			t.Fatal(err)
		}
	}
	reconcileOnce := func() reconcile.Result {
		t.Helper()
		res, err := r.Reconcile(ctx, reconcile.Request{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	ackOf := func() StopAck {
		var cm corev1.ConfigMap
		if err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: StopsAckConfigMap}, &cm); err != nil {
			t.Fatal(err)
		}
		a, ok := ParseStopAck(&cm)
		if !ok {
			t.Fatalf("the acknowledgment does not parse: %v", cm.Data)
		}
		return a
	}
	policy := func(k int) (*unstructured.Unstructured, error) {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(AgentgatewayPolicyGVK)
		return o, envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: fmt.Sprintf("ambit-agent-stops-%d", k)}, o)
	}
	// agentgateway's verdict on a policy, for the given routes
	accept := func(k int, routes ...string) {
		t.Helper()
		o, err := policy(k)
		if err != nil {
			t.Fatal(err)
		}
		var anc []any
		for _, rt := range routes {
			c := func(typ string) any {
				return map[string]any{"type": typ, "status": "True", "observedGeneration": o.GetGeneration(), "reason": "Accepted", "message": "ok", "lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}
			}
			anc = append(anc, map[string]any{"controllerName": agentgatewayController, "ancestorRef": map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": rt},
				"conditions": []any{c("Accepted"), c("Attached")}})
		}
		_ = unstructured.SetNestedSlice(o.Object, anc, "status", "ancestors")
		if err := envClient.Status().Update(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	// no list: nothing happens
	if res := reconcileOnce(); res.RequeueAfter != 0 {
		t.Errorf("no list: %+v", res)
	}
	// 151 agents: two policies on the three routes; nothing accepted yet: no Gateway is acknowledged
	list(1, 151)
	if res := reconcileOnce(); res.RequeueAfter == 0 {
		t.Error("an unacknowledged stop is asked again")
	}
	for k := 0; k < 2; k++ {
		p, err := policy(k)
		if err != nil {
			t.Fatalf("policy %d: %v", k, err)
		}
		if refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs"); len(refs) != 3 {
			t.Errorf("policy %d targets %v", k, refs)
		}
	}
	if a := ackOf(); a.Gen != 1 || len(a.Gateways) != 2 || a.Gateways["ambit"].AckedGen != 0 || a.Gateways["ambit-public"].AckedGen != 0 {
		t.Fatalf("before any verdict: %+v", a)
	}
	// partial rollout: the private Gateway's routes accepted, the public one's not: only "ambit" is acknowledged
	accept(0, "ambit-mcp", "ambit-llm")
	accept(1, "ambit-mcp", "ambit-llm")
	if res := reconcileOnce(); res.RequeueAfter == 0 {
		t.Error("a partial rollout is asked again")
	}
	if a := ackOf(); a.Gateways["ambit"].AckedGen != 1 || a.Gateways["ambit-public"].AckedGen != 0 {
		t.Fatalf("partial rollout: %+v", a)
	}
	// only one of the two policies accepted on the public Gateway: still not acknowledged
	accept(0, "ambit-mcp", "ambit-llm", "ambit-mcp-public")
	reconcileOnce()
	if a := ackOf(); a.Gateways["ambit-public"].AckedGen != 0 {
		t.Fatalf("one of two policies on the public Gateway: %+v", a)
	}
	accept(1, "ambit-mcp", "ambit-llm", "ambit-mcp-public")
	if res := reconcileOnce(); res.RequeueAfter != 30*time.Second { // confirmed: only the safety-net pass remains
		t.Errorf("all acknowledged: %+v", res)
	}
	if a := ackOf(); a.Gateways["ambit"].AckedGen != 1 || a.Gateways["ambit-public"].AckedGen != 1 || a.Gateways["ambit-public"].At.IsZero() {
		t.Fatalf("all accepted: %+v", a)
	}

	// a longer list: the new generation is not acknowledged until the policies carry it and are accepted again
	list(2, 160)
	reconcileOnce()
	if a := ackOf(); a.Gen != 2 || a.Gateways["ambit"].AckedGen != 1 {
		t.Fatalf("new generation, no verdict yet: %+v", a)
	}
	p0, _ := policy(0)
	if p0.GetAnnotations()[StopsGenAnnotation] != "2" {
		t.Fatalf("policy 0 carries generation %s", p0.GetAnnotations()[StopsGenAnnotation])
	}
	accept(0, "ambit-mcp", "ambit-llm", "ambit-mcp-public")
	accept(1, "ambit-mcp", "ambit-llm", "ambit-mcp-public")
	reconcileOnce()
	if a := ackOf(); a.Gateways["ambit"].AckedGen != 2 || a.Gateways["ambit-public"].AckedGen != 2 {
		t.Fatalf("generation 2: %+v", a)
	}

	// drift: a stop policy is edited by hand (or deleted) while the list stays: the Gateway is not ready until it is put
	// back and accepted again; the acknowledged generation stays (it only rises), but Ready is what the console checks
	if a := ackOf(); !a.Gateways["ambit"].Ready || a.ListUID == "" {
		t.Fatalf("ready before the drift: %+v", a)
	}
	drift, _ := policy(0)
	_ = unstructured.SetNestedSlice(drift.Object, []any{"false"}, "spec", "traffic", "authorization", "policy", "matchExpressions")
	if err := envClient.Update(ctx, drift); err != nil {
		t.Fatal(err)
	}
	reconcileOnce() // the controller puts its own spec back; the new generation has no verdict yet
	if a := ackOf(); a.Gateways["ambit"].Ready || a.Gateways["ambit-public"].Ready {
		t.Fatalf("a changed policy is still ready: %+v", a)
	}
	accept(0, "ambit-mcp", "ambit-llm", "ambit-mcp-public")
	reconcileOnce()
	if a := ackOf(); !a.Gateways["ambit"].Ready {
		t.Fatalf("not ready after the verdict: %+v", a)
	}
	// a stop list that is recreated is a new list: the old answer does not count for it
	oldUID := ackOf().ListUID
	var lcm corev1.ConfigMap
	_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: StopsConfigMap}, &lcm)
	_ = envClient.Delete(ctx, &lcm)
	list(2, 160)
	reconcileOnce()
	if a := ackOf(); a.ListUID == oldUID || a.Gateways["ambit"].AckedGen != 0 && a.Gateways["ambit"].AckedGen != 2 {
		t.Fatalf("a recreated list kept the old answer: %+v", a)
	}
	accept(0, "ambit-mcp", "ambit-llm", "ambit-mcp-public")
	accept(1, "ambit-mcp", "ambit-llm", "ambit-mcp-public")
	reconcileOnce()

	// 1,001 entries: not rendered; the last policies stay, the answer says why
	list(3, 1001)
	reconcileOnce()
	if a := ackOf(); a.Degraded == "" || a.Gateways["ambit"].AckedGen != 2 {
		t.Fatalf("too long a list: %+v", a)
	}
	if p, err := policy(0); err != nil || p.GetAnnotations()[StopsGenAnnotation] != "2" {
		t.Fatalf("the policy in force changed: %v", err)
	}
	// an unreadable list: degraded, policies stay
	bad := &corev1.ConfigMap{}
	_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: StopsConfigMap}, bad)
	bad.Data = map[string]string{"list.json": "{not json"}
	_ = envClient.Update(ctx, bad)
	reconcileOnce()
	if a := ackOf(); a.Degraded == "" {
		t.Fatalf("unreadable list: %+v", a)
	}

	// the list gets shorter: the second policy goes, the first stays and no longer lists the pruned names
	list(4, 3)
	reconcileOnce()
	if _, err := policy(1); err == nil {
		t.Error("policy 1 is still there")
	}
	p0, err := policy(0)
	if err != nil {
		t.Fatal(err)
	}
	if e, _, _ := unstructured.NestedStringSlice(p0.Object, "spec", "traffic", "authorization", "policy", "matchExpressions"); len(e[0]) > 300 {
		t.Errorf("policy 0 still lists many names: %d characters", len(e[0]))
	}
	// an empty list: one policy that allows everyone, so a policy that once existed stays
	list(5, 0)
	reconcileOnce()
	p0, err = policy(0)
	if err != nil {
		t.Fatal(err)
	}
	if e, _, _ := unstructured.NestedStringSlice(p0.Object, "spec", "traffic", "authorization", "policy", "matchExpressions"); e[0] != "true" {
		t.Errorf("empty list: %v", e)
	}
}
