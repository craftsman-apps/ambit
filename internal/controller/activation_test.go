package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var gateStart = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

// gateReconciler has a fake clock that only the gate's sleeps move.
func gateReconciler(objs ...client.Object) (*Reconciler, *time.Time) {
	clock := gateStart
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	r := &Reconciler{Client: c, Live: c, Namespace: "ambit-system", Now: func() time.Time { return clock },
		Activation: &Activation{Timeout: 120 * time.Second, Settle: 3 * time.Second,
			Sleep: func(_ context.Context, d time.Duration) error { clock = clock.Add(d); return nil }}}
	return r, &clock
}

func agwCond(typ, status, reason string, gen int64) any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": "m-" + reason, "observedGeneration": gen,
		"lastTransitionTime": gateStart.Format(time.RFC3339)}
}

func gatedPolicy(gen int64, ancestors ...any) *unstructured.Unstructured {
	p := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "agentgateway.dev/v1alpha1", "kind": "AgentgatewayPolicy",
		"metadata": map[string]any{"name": AccessLogPolicy, "namespace": "ambit-system", "generation": gen},
		"status":   map[string]any{"ancestors": ancestors}}}
	return p
}

func ancestor(controller string, conds ...any) any {
	return map[string]any{"controllerName": controller, "ancestorRef": map[string]any{"name": "ambit"}, "conditions": conds}
}

func gatedModel(gen int64, conds ...any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "agentgateway.dev/v1alpha1", "kind": "AgentgatewayModel",
		"metadata": map[string]any{"name": "gpt", "namespace": "ambit-system", "generation": gen},
		"status":   map[string]any{"parents": []any{map[string]any{"controllerName": agentgatewayController, "conditions": conds}}}}}
}

func nack(at time.Time) *corev1.Event {
	return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "nack-" + at.Format("150405"), Namespace: "ambit-system"},
		InvolvedObject: corev1.ObjectReference{Kind: "Gateway", Name: "ambit", Namespace: "ambit-system"},
		Type:           corev1.EventTypeWarning, Reason: "AgentGatewayNackError", Message: "proxy rejected the update: bad CEL",
		LastTimestamp: metav1.NewTime(at)}
}

func TestGatePassesWhenAgentgatewayAcceptedAndAttachedTheGeneration(t *testing.T) {
	p := gatedPolicy(4, ancestor(agentgatewayController, agwCond("Accepted", "True", "Valid", 4), agwCond("Attached", "True", "Attached", 5)),
		ancestor("other.example/controller", agwCond("Accepted", "False", "Invalid", 4))) // another controller's view does not count
	r, clock := gateReconciler(p)
	if err := r.gate(t.Context(), []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 4}}, nil, 120*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := clock.Sub(gateStart); got != 3*time.Second {
		t.Errorf("waited %v, want the settle time 3s", got)
	}
}

func TestGateWaitsForTheNewGenerationThenTimesOut(t *testing.T) {
	p := gatedPolicy(5, ancestor(agentgatewayController, agwCond("Accepted", "True", "Valid", 4), agwCond("Attached", "True", "Attached", 4)))
	r, clock := gateReconciler(p)
	err := r.gate(t.Context(), []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 5}}, nil, 120*time.Second)
	if err == nil {
		t.Fatal("passed on an older generation")
	}
	if got := clock.Sub(gateStart); got < 120*time.Second || got > 122*time.Second {
		t.Errorf("gave up after %v, want the timeout 120s", got)
	}
	want := "waiting for agentgateway to apply AgentgatewayPolicy/ambit-access-log generation 5: Accepted is True (Valid: m-Valid) for generation 4"
	if err.Error() != want {
		t.Errorf("error\n got %s\nwant %s", err, want)
	}
}

func TestGateWaitsForAnyStatus(t *testing.T) {
	r, _ := gateReconciler(gatedPolicy(1))
	err := r.gate(t.Context(), []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 1}}, nil, 120*time.Second)
	if err == nil || !strings.HasSuffix(err.Error(), "generation 1: Accepted is missing") {
		t.Fatalf("error %v", err)
	}
}

func TestGateFailsAtOnceOnInvalid(t *testing.T) {
	p := gatedPolicy(2, ancestor(agentgatewayController, agwCond("Accepted", "False", "Invalid", 2), agwCond("Attached", "False", "Pending", 2)))
	r, clock := gateReconciler(p)
	err := r.gate(t.Context(), []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 2}}, nil, 120*time.Second)
	if err == nil || err.Error() != "waiting for agentgateway to apply AgentgatewayPolicy/ambit-access-log generation 2: Accepted is False (Invalid: m-Invalid)" {
		t.Fatalf("error %v", err)
	}
	if clock.Sub(gateStart) > 0 {
		t.Errorf("waited %v before failing", clock.Sub(gateStart))
	}
	// An Invalid verdict on an older generation is not about this write: wait for the new one.
	old := gatedPolicy(3, ancestor(agentgatewayController, agwCond("Accepted", "False", "Invalid", 2)))
	r, clock = gateReconciler(old)
	_ = r.gate(t.Context(), []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 3}}, nil, 120*time.Second)
	if clock.Sub(gateStart) < 120*time.Second {
		t.Errorf("failed at once on an older generation's verdict")
	}
}

func TestGateFailsAtOnceOnANackAfterTheWrite(t *testing.T) {
	ok := gatedPolicy(1, ancestor(agentgatewayController, agwCond("Accepted", "True", "Valid", 1), agwCond("Attached", "True", "Attached", 1)))
	objs := []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 1}}
	r, _ := gateReconciler(ok, nack(gateStart.Add(-time.Minute))) // before the write: an earlier change's
	seen, err := r.nacksSeen(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.gate(t.Context(), objs, seen, 120*time.Second); err != nil {
		t.Fatalf("an older NACK failed the gate: %v", err)
	}
	// The recorder stores whole seconds: a NACK 300 ms after a write at 10:00:00.400 says 10:00:00. Clocks are
	// not compared; an Event that was not there before the write counts.
	r, clock := gateReconciler(ok)
	seen, _ = r.nacksSeen(t.Context())
	if err := r.Create(t.Context(), nack(gateStart.Truncate(time.Second))); err != nil {
		t.Fatal(err)
	}
	err = r.gate(t.Context(), objs, seen, 120*time.Second)
	if err == nil || err.Error() != "a gateway pod rejected the update (AgentGatewayNackError): proxy rejected the update: bad CEL" {
		t.Fatalf("error %v", err)
	}
	if clock.Sub(gateStart) > 0 {
		t.Errorf("waited %v before failing", clock.Sub(gateStart))
	}
	// The recorder folds a repeated NACK into the same Event and raises its count.
	old := nack(gateStart.Add(-time.Minute))
	old.Count = 2
	r, _ = gateReconciler(ok, old)
	seen, _ = r.nacksSeen(t.Context())
	old.Count = 3
	if err := r.Update(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := r.gate(t.Context(), objs, seen, 120*time.Second); err == nil {
		t.Fatal("a NACK repeated after the write (count 2 -> 3) did not fail the gate")
	}
}

func TestGateOnAModelNeedsAcceptedOnEveryParent(t *testing.T) {
	r, _ := gateReconciler(gatedModel(2, agwCond("Accepted", "True", "Accepted", 2)))
	if err := r.gate(t.Context(), []gated{{kind: "AgentgatewayModel", name: "gpt", generation: 2}}, nil, 120*time.Second); err != nil {
		t.Fatal(err)
	}
	r, _ = gateReconciler(gatedModel(3, agwCond("Accepted", "True", "Accepted", 2)))
	if err := r.gate(t.Context(), []gated{{kind: "AgentgatewayModel", name: "gpt", generation: 3}}, nil, 120*time.Second); err == nil ||
		!strings.HasPrefix(err.Error(), "waiting for agentgateway to apply AgentgatewayModel/gpt generation 3: Accepted is True") {
		t.Fatalf("error %v", err)
	}
}

func TestGateWithNothingWrittenDoesNotWait(t *testing.T) {
	r, clock := gateReconciler()
	if err := r.gate(t.Context(), nil, nil, 120*time.Second); err != nil || clock.Sub(gateStart) != 0 {
		t.Fatalf("err %v, waited %v", err, clock.Sub(gateStart))
	}
}

// --activation-timeout 0 is refused: many tools read 0 as "no limit", and here it would mean "no gate at all".
func TestActivationFlagsAreChecked(t *testing.T) {
	for _, c := range []struct {
		timeout, settle time.Duration
		want            string
	}{
		{120 * time.Second, 3 * time.Second, ""},
		{time.Second, 0, ""},
		{0, 3 * time.Second, "--activation-timeout must be more than 0 (got 0s)"},
		{-time.Second, 3 * time.Second, "--activation-timeout must be more than 0 (got -1s)"},
		{120 * time.Second, -time.Second, "--activation-settle must not be negative (got -1s)"},
	} {
		err := (&Activation{Timeout: c.timeout, Settle: c.settle}).Check()
		if got := fmt.Sprint(err); (c.want == "" && err != nil) || (c.want != "" && got != c.want) {
			t.Errorf("timeout %v settle %v: %v, want %q", c.timeout, c.settle, err, c.want)
		}
	}
}
