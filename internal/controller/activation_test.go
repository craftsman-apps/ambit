package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
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
	// The message names no object: say so, and give the general next step.
	want := "a gateway pod rejected the update (AgentGatewayNackError): proxy rejected the update: bad CEL. " +
		"agentgateway does not say which object; check the provider secret of each Model."
	var st *stalledError
	if !errors.As(err, &st) || st.msg != want || len(st.objects) != 0 {
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

// ownedAgentgatewayModel is what Ambit generates for a Model (or, with ownerKind ModelRouter, for a router): the
// AgentgatewayModel has the Model's name and is owned by it.
func ownedAgentgatewayModel(name, ownerKind string) *unstructured.Unstructured {
	o := gatedModel(1)
	o.SetName(name)
	o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "ambit.dev/v1", Kind: ownerKind, Name: name, UID: "u-" + types.UID(name), Controller: ptr.To(true)}})
	return o
}

// nackJSON is the text agentgateway sends in a NACK: a JSON array with one {key, error} entry per rejected resource,
// the key being "<kind>/<namespace>/<name>" (crates/xds/src/client.rs:76-82 and :715; keys such as
// "backend/default/bad-gcp" in crates/agentgateway/src/store/binds.rs:2603).
func nackJSON(entries ...[2]string) string {
	var out []map[string]string
	for _, e := range entries {
		out = append(out, map[string]string{"key": e[0], "error": e[1]})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func nackAbout(name, message string) *corev1.Event {
	e := nack(gateStart.Truncate(time.Second))
	e.Name, e.Message = name, message
	return e
}

// stallOf runs the gate against one NACK text and the given AgentgatewayModels, and returns the stop.
func stallOf(t *testing.T, text string, models ...*unstructured.Unstructured) *stalledError {
	t.Helper()
	ok := gatedPolicy(1, ancestor(agentgatewayController, agwCond("Accepted", "True", "Valid", 1), agwCond("Attached", "True", "Attached", 1)))
	objs := []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 1}}
	r, _ := gateReconciler(append([]client.Object{ok}, toObjects(models)...)...)
	seen, _ := r.nacksSeen(t.Context())
	if err := r.Create(t.Context(), nackAbout("nack-1", text)); err != nil {
		t.Fatal(err)
	}
	var st *stalledError
	if err := r.gate(t.Context(), objs, seen, 120*time.Second); !errors.As(err, &st) {
		t.Fatalf("error %v", err)
	}
	return st
}

func toObjects(in []*unstructured.Unstructured) []client.Object {
	out := make([]client.Object, len(in))
	for i, o := range in {
		out[i] = o
	}
	return out
}

// A stalled change names the rejected Model, the gateway's reason, and what to do.
func TestGateNamesTheRejectedModel(t *testing.T) {
	text := nackJSON([2]string{"backend/ambit-system/mistral-x", "GCP credential JSON missing `type` field"})
	st := stallOf(t, text, ownedAgentgatewayModel("gpt", "Model"), ownedAgentgatewayModel("mistral-x", "Model"))
	want := "agentgateway rejected Model/mistral-x: backend/ambit-system/mistral-x: GCP credential JSON missing `type` field. Check the provider secret of Model/mistral-x, or remove the Model."
	if st.msg != want || fmt.Sprint(st.objects) != "[Model/mistral-x]" {
		t.Errorf("message %q objects %v\nwant %q", st.msg, st.objects, want)
	}
}

func TestGateNamesTwoRejectedObjects(t *testing.T) {
	text := nackJSON([2]string{"backend/ambit-system/mistral-x", "bad key"}, [2]string{"model/ambit-system/gpt", "bad key"})
	st := stallOf(t, text, ownedAgentgatewayModel("gpt", "Model"), ownedAgentgatewayModel("mistral-x", "Model"),
		ownedAgentgatewayModel("fast", "ModelRouter"), ownedAgentgatewayModel("llama", "Model"))
	want := "agentgateway rejected Model/gpt and Model/mistral-x: backend/ambit-system/mistral-x: bad key; model/ambit-system/gpt: bad key. Check the provider secret of each rejected Model, or remove the Model."
	if st.msg != want || fmt.Sprint(st.objects) != "[Model/gpt Model/mistral-x]" {
		t.Errorf("message %q objects %v\nwant %q", st.msg, st.objects, want)
	}
}

// Only the key of a rejected resource names an object. A name that is part of a longer one, or a word in the
// reason, never does.
func TestGateNeverNamesTheWrongObject(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"gpt-4 is not gpt-4.1", nackJSON([2]string{"backend/ambit-system/gpt-4.1", "bad key"}), "[Model/gpt-4.1]"},
		{"gpt-4.1 is not gpt-4", nackJSON([2]string{"backend/ambit-system/gpt-4", "bad key"}), "[Model/gpt-4]"},
		{"words of the reason", nackJSON([2]string{"backend/ambit-system/mistral-x", "GCP credential JSON missing `type` field of backend"}), "[Model/mistral-x]"},
		{"a file name in the reason", nackJSON([2]string{"backend/ambit-system/gpt", "cannot read mistral-x.json"}), "[Model/gpt]"},
		{"another namespace", nackJSON([2]string{"backend/other/gpt", "bad key"}), "[]"},
		{"not JSON", "invalid backend for gpt-4 and gpt-4.1", "[]"},
		{"a key without a name", nackJSON([2]string{"backend/ambit-system", "bad key"}), "[]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var models []*unstructured.Unstructured
			for _, n := range []string{"gpt", "gpt-4", "gpt-4.1", "mistral-x", "type", "credential", "field", "backend", "json"} {
				models = append(models, ownedAgentgatewayModel(n, "Model"))
			}
			st := stallOf(t, c.text, models...)
			if fmt.Sprint(st.objects) != c.want {
				t.Errorf("objects %v, want %s", st.objects, c.want)
			}
			if c.want == "[]" && !strings.Contains(st.msg, "does not say which object") {
				t.Errorf("no generic message: %q", st.msg)
			}
		})
	}
}

// The reason is cut to 300 characters in the message; the Event keeps the whole text.
func TestGateBoundsTheReason(t *testing.T) {
	long := strings.Repeat("é", 1000)
	st := stallOf(t, nackJSON([2]string{"backend/ambit-system/gpt", long}), ownedAgentgatewayModel("gpt", "Model"))
	if n := len([]rune(st.msg)); n > 500 || !strings.Contains(st.msg, "…") {
		t.Errorf("message of %d characters: %q", n, st.msg)
	}
	if !strings.HasPrefix(st.msg, "agentgateway rejected Model/gpt: ") {
		t.Errorf("message %q", st.msg)
	}
}

// When the Models cannot be listed the text says so, instead of blaming the gateway.
func TestGateSaysWhenTheModelsCannotBeListed(t *testing.T) {
	ok := gatedPolicy(1, ancestor(agentgatewayController, agwCond("Accepted", "True", "Valid", 1), agwCond("Attached", "True", "Attached", 1)))
	r, _ := gateReconciler(ok)
	r.Live = failingList{r.Live}
	seen, _ := r.nacksSeen(t.Context())
	_ = r.Create(t.Context(), nackAbout("nack-1", nackJSON([2]string{"backend/ambit-system/gpt", "bad key"})))
	var st *stalledError
	err := r.gate(t.Context(), []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 1}}, seen, 120*time.Second)
	if !errors.As(err, &st) || !strings.Contains(st.msg, "could not work out which object") || strings.Contains(st.msg, "does not say") {
		t.Errorf("message %v", err)
	}
}

type failingList struct{ client.Reader }

func (f failingList) List(ctx context.Context, l client.ObjectList, o ...client.ListOption) error {
	if _, isModels := l.(*unstructured.UnstructuredList); !isModels {
		return f.Reader.List(ctx, l, o...)
	}
	return errors.New("list failed")
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

// An object that a step wrote and that was deleted before agentgateway reported on it (deleting a Model removes its
// AgentgatewayModel) has nothing left to apply. Waiting for it held the single worker for the whole timeout.
func TestGateDoesNotWaitForAnObjectThatWasDeleted(t *testing.T) {
	r, clock := gateReconciler() // the object is in neither the cache nor the API server
	if err := r.gate(t.Context(), []gated{{kind: "AgentgatewayModel", name: "gone", generation: 3}}, nil, 120*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := clock.Sub(gateStart); got != 3*time.Second {
		t.Errorf("waited %v, want the settle time 3s", got)
	}
}

// An object that the API server has but the cache has not seen yet is still waited for.
func TestGateWaitsForAnObjectThatIsNotInTheCacheYet(t *testing.T) {
	r, _ := gateReconciler()
	r.Live = fake.NewClientBuilder().WithObjects(gatedModel(3)).Build()
	err := r.gate(t.Context(), []gated{{kind: "AgentgatewayModel", name: "gpt", generation: 3}}, nil, 120*time.Second)
	if err == nil || !strings.HasSuffix(err.Error(), "generation 3: the object is missing") {
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
