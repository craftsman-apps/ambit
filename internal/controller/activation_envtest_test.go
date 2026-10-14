package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// playAgentgateway writes agentgateway's verdict on the object's current generation, as its controller would.
func playAgentgateway(t *testing.T, ns, kind, name string) {
	t.Helper()
	o, err := genGet(t.Context(), ns, kind, name)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	c := func(typ string) any {
		return map[string]any{"type": typ, "status": "True", "reason": typ, "message": "ok", "observedGeneration": o.GetGeneration(), "lastTransitionTime": now}
	}
	if kind == "AgentgatewayModel" {
		_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"controllerName": agentgatewayController,
			"parentRef": map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "ambit-llm"}, "conditions": []any{c("Accepted")}}}, "status", "parents")
	} else {
		_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"controllerName": agentgatewayController,
			"ancestorRef": map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "ambit"}, "conditions": []any{c("Accepted"), c("Attached")}}}, "status", "ancestors")
	}
	if err := envClient.Status().Update(t.Context(), o); err != nil {
		t.Fatal(err)
	}
}

func pendingOf(t *testing.T, ns string) map[string]string {
	t.Helper()
	d, _ := accessMode(t.Context(), ns)
	if d["pending"] == "" {
		return nil
	}
	var p map[string]string
	if err := json.Unmarshal([]byte(d["pending"]), &p); err != nil {
		t.Fatalf("pending %q: %v", d["pending"], err)
	}
	return p
}

// stays checks for a while that a condition keeps holding: the next step must not move before the status.
func stays(t *testing.T, what string, f func() error) {
	t.Helper()
	for end := time.Now().Add(500 * time.Millisecond); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if err := f(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
}

func reconcileAsync(r *Reconciler) chan error {
	done := make(chan error, 1)
	go func() { _, err := r.Reconcile(context.Background(), reconcile.Request{}); done <- err }()
	return done
}

func wait(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("reconcile did not finish")
	}
}

func ruleReady(t *testing.T, ns, name string) metav1.Condition {
	t.Helper()
	var ar v1.AccessRule
	if err := envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ar); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(ar.Status.Conditions, "Ready"); c != nil {
		return *c
	}
	return metav1.Condition{}
}

// Enforce mode: authorization first, then the access log; each waits for agentgateway's status, and the rules
// version is published only after the last one. A step that never becomes active stops the change there.
func TestRulesChangeStepsWaitForAgentgateway(t *testing.T) {
	needEnv(t)
	ns := "ambit-gate-enforce"
	namespace(t, ns)
	ctx := t.Context()
	createBase(t, ns)
	for _, o := range []client.Object{
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-k"}}},
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"gemma"}}},
	} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"},
		Activation: Activation{Timeout: 10 * time.Second, Poll: 50 * time.Millisecond}}
	done := reconcileAsync(r)
	eventually(t, "authorization written", func() error { _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); return err })
	stays(t, "access log waits for authorization", func() error {
		if _, err := accessLogOf(ctx, ns); !apierrors.IsNotFound(err) {
			return fmt.Errorf("access log written: %v", err)
		}
		if d, _ := accessMode(ctx, ns); d["mode"] != "" || d["rulesVersion"] != "" {
			return fmt.Errorf("published early: %v", d)
		}
		if p := pendingOf(t, ns); p["step"] != "authorization" || p["mode"] != "enforce" || p["error"] != "" {
			return fmt.Errorf("pending %v", p)
		}
		return nil
	})
	playAgentgateway(t, ns, "AgentgatewayModel", "gemma")
	eventually(t, "access log written", func() error { _, err := accessLogOf(ctx, ns); return err })
	stays(t, "published only after the access log is active", func() error {
		if d, _ := accessMode(ctx, ns); d["rulesVersion"] != "" {
			return fmt.Errorf("published early: %v", d)
		}
		if p := pendingOf(t, ns); p["step"] != "access log" {
			return fmt.Errorf("pending %v", p)
		}
		return nil
	})
	playAgentgateway(t, ns, "AgentgatewayPolicy", AccessLogPolicy)
	wait(t, done)
	first, _ := accessMode(ctx, ns)
	if first["mode"] != "enforce" || len(first["rulesVersion"]) != 12 || first["pending"] != "" {
		t.Fatalf("published %v", first)
	}

	// A rules change whose authorization agentgateway never applies: stopped at that step, old values kept.
	r.Activation.Timeout = 300 * time.Millisecond
	var ar v1.AccessRule
	_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "eng"}, &ar)
	ar.Spec.Groups = []string{"eng", "ops"}
	if err := envClient.Update(ctx, &ar); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(ctx, reconcile.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("a stalled change is not retried: %v", res)
	}
	c := ruleReady(t, ns, "eng")
	if c.Status != metav1.ConditionFalse || c.Reason != "RulesNotActive" ||
		c.Message != "waiting for agentgateway to apply AgentgatewayModel/gemma generation 2: Accepted is True (Accepted: ok) for generation 1" {
		t.Errorf("rule condition %+v", c)
	}
	if d, _ := accessMode(ctx, ns); d["rulesVersion"] != first["rulesVersion"] || d["rulesSince"] != first["rulesSince"] {
		t.Errorf("published %v, want the old %v", d, first)
	}
	if p := pendingOf(t, ns); p["step"] != "authorization" || !strings.Contains(p["error"], "AgentgatewayModel/gemma generation 2") || p["rulesVersion"] == first["rulesVersion"] {
		t.Errorf("pending %v", p)
	}
	if p, _ := accessLogOf(ctx, ns); attributes(p)["ambit.rulesVersion"] != `"`+first["rulesVersion"]+`"` {
		t.Errorf("the access log moved past the stopped step: %v", attributes(p))
	}

	// agentgateway catches up: the retry resumes from the stopped step (the model is not written again).
	playAgentgateway(t, ns, "AgentgatewayModel", "gemma")
	r.Activation.Timeout = 10 * time.Second
	done = reconcileAsync(r)
	eventually(t, "access log with the new version", func() error {
		p, err := accessLogOf(ctx, ns)
		if err == nil && attributes(p)["ambit.rulesVersion"] == `"`+first["rulesVersion"]+`"` {
			return fmt.Errorf("old version")
		}
		return err
	})
	playAgentgateway(t, ns, "AgentgatewayPolicy", AccessLogPolicy)
	wait(t, done)
	if d, _ := accessMode(ctx, ns); d["rulesVersion"] == first["rulesVersion"] || d["pending"] != "" {
		t.Errorf("published %v", d)
	}
	if c := ruleReady(t, ns, "eng"); c.Status != metav1.ConditionTrue {
		t.Errorf("rule condition after the change became active: %+v", c)
	}

	// A mode switch that stops: ModeSwitchStalled, the old mode kept.
	r.AccessMode, r.Activation.Timeout = "audit", 300*time.Millisecond
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if c := ruleReady(t, ns, "eng"); c.Reason != "ModeSwitchStalled" || !strings.HasPrefix(c.Message, "waiting for agentgateway to apply AgentgatewayPolicy/ambit-access-log generation 3: ") {
		t.Errorf("rule condition %+v", c)
	}
	if d, _ := accessMode(ctx, ns); d["mode"] != "enforce" {
		t.Errorf("published %v", d)
	}
	if p := pendingOf(t, ns); p["mode"] != "audit" || p["step"] != "access log" {
		t.Errorf("pending %v", p)
	}
}

// Audit mode: the access log first, then authorization; nothing is written before the access log is active.
func TestAuditOrderWritesTheAccessLogFirst(t *testing.T) {
	needEnv(t)
	ns := "ambit-gate-audit"
	namespace(t, ns)
	ctx := t.Context()
	createBase(t, ns)
	for _, o := range []client.Object{
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-k"}}},
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"gemma"}}},
	} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}, AccessMode: "audit",
		Activation: Activation{Timeout: 10 * time.Second, Poll: 50 * time.Millisecond}}
	done := reconcileAsync(r)
	eventually(t, "access log written", func() error { _, err := accessLogOf(ctx, ns); return err })
	stays(t, "authorization waits for the access log", func() error {
		if _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); !apierrors.IsNotFound(err) {
			return fmt.Errorf("authorization written: %v", err)
		}
		return nil
	})
	if p, _ := accessLogOf(ctx, ns); attributes(p)["ambit.accessMode"] != `"audit"` {
		t.Fatalf("attributes %v", attributes(p))
	}
	playAgentgateway(t, ns, "AgentgatewayPolicy", AccessLogPolicy)
	eventually(t, "authorization written", func() error { _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); return err })
	playAgentgateway(t, ns, "AgentgatewayModel", "gemma")
	wait(t, done)
	if d, _ := accessMode(ctx, ns); d["mode"] != "audit" || d["pending"] != "" {
		t.Fatalf("published %v", d)
	}
}

func gateFixture(t *testing.T, ns string) {
	t.Helper()
	namespace(t, ns)
	createBase(t, ns)
	for _, o := range []client.Object{
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-k"}}},
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"gemma"}}},
	} {
		if err := envClient.Create(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
}

func offboard(t *testing.T, ns string, emails string) {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(t.Context(), envClient, cm, func() error {
		cm.Data = map[string]string{"emails": emails}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func offboardedExpr(ctx context.Context, ns string) string {
	p, err := genGet(ctx, ns, "AgentgatewayPolicy", "ambit-offboarded")
	if err != nil {
		return ""
	}
	e, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
	return strings.Join(e, " ")
}

// A change that waits for agentgateway never holds back what must apply at once: offboarding, deletions, and the
// access-log policy written again after a delete. Audit order: the access log is the first step, authorization
// (where offboarding lives) waits behind it.
func TestWaitingChangeStillOffboardsAndKeepsTheAccessLog(t *testing.T) {
	needEnv(t)
	ns := "ambit-gate-urgent"
	gateFixture(t, ns)
	ctx := t.Context()
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}, AccessMode: "audit",
		Activation: Activation{Timeout: 10 * time.Second, Poll: 50 * time.Millisecond}}
	done := reconcileAsync(r)
	eventually(t, "access log written", func() error { _, err := accessLogOf(ctx, ns); return err })
	// While the access-log step waits: a leaver is offboarded, and someone deletes the access-log policy.
	offboard(t, ns, `["leaver@example.com"]`)
	eventually(t, "offboarding applied while the change waits", func() error {
		if e := offboardedExpr(ctx, ns); !strings.Contains(e, `"leaver@example.com"`) {
			return fmt.Errorf("offboarding policy %q", e)
		}
		return nil
	})
	if err := envClient.Delete(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "agentgateway.dev/v1alpha1",
		"kind": "AgentgatewayPolicy", "metadata": map[string]any{"name": AccessLogPolicy, "namespace": ns}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "access log written again while the change waits", func() error {
		p, err := accessLogOf(ctx, ns)
		if err == nil && p.GetDeletionTimestamp() != nil {
			return fmt.Errorf("still being deleted")
		}
		return err
	})
	if _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); !apierrors.IsNotFound(err) {
		t.Fatalf("authorization written before the access log was active: %v", err)
	}
	playAgentgateway(t, ns, "AgentgatewayPolicy", AccessLogPolicy)
	eventually(t, "authorization written", func() error { _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); return err })
	playAgentgateway(t, ns, "AgentgatewayModel", "gemma")
	wait(t, done)
}

// Audit order, the access-log step stalled: authorization does not move, but offboarding and the deletion of
// objects no longer generated still happen in the same pass.
func TestStalledAuditChangeStillOffboardsAndDeletes(t *testing.T) {
	needEnv(t)
	ns := "ambit-gate-stalled-audit"
	gateFixture(t, ns)
	ctx := t.Context()
	stale := generated("agentgateway.dev/v1alpha1", "AgentgatewayPolicy", BudgetPolicyName, ns, map[string]any{
		"targetRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "ambit-llm"}},
		"traffic":    map[string]any{"authorization": map[string]any{"action": "Require", "policy": map[string]any{"matchExpressions": []any{"true"}}}}})
	if err := envClient.Create(ctx, stale); err != nil {
		t.Fatal(err)
	}
	offboard(t, ns, `["leaver@example.com"]`)
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}, AccessMode: "audit",
		Activation: Activation{Timeout: 300 * time.Millisecond, Poll: 50 * time.Millisecond}}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if c := ruleReady(t, ns, "eng"); c.Reason != "RulesNotActive" {
		t.Fatalf("rule condition %+v, want the change stalled", c)
	}
	if _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); !apierrors.IsNotFound(err) {
		t.Fatalf("authorization written before the access log was active: %v", err)
	}
	if e := offboardedExpr(ctx, ns); !strings.Contains(e, `"leaver@example.com"`) {
		t.Errorf("offboarding policy %q", e)
	}
	if _, err := genGet(ctx, ns, "AgentgatewayPolicy", BudgetPolicyName); !apierrors.IsNotFound(err) {
		t.Errorf("the budget policy no longer generated is still there: %v", err)
	}
	// The next pass, still stalled, offboards a second leaver at once: the retry does not wait the timeout again.
	offboard(t, ns, `["leaver@example.com","second@example.com"]`)
	r.Activation.Timeout = time.Minute
	start := time.Now()
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("a retry of the stalled step waited %v", d)
	}
	if e := offboardedExpr(ctx, ns); !strings.Contains(e, `"second@example.com"`) {
		t.Errorf("offboarding policy %q", e)
	}
}

// Enforce order, the authorization step stalled: the access-log policy deleted meanwhile is written again with the
// published (in force) mode and rules version.
func TestStalledEnforceChangeWritesTheDeletedAccessLogAgain(t *testing.T) {
	needEnv(t)
	ns := "ambit-gate-stalled-enforce"
	gateFixture(t, ns)
	ctx := t.Context()
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"},
		Activation: Activation{Timeout: 10 * time.Second, Poll: 50 * time.Millisecond}}
	done := reconcileAsync(r)
	eventually(t, "authorization written", func() error { _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); return err })
	playAgentgateway(t, ns, "AgentgatewayModel", "gemma")
	eventually(t, "access log written", func() error { _, err := accessLogOf(ctx, ns); return err })
	playAgentgateway(t, ns, "AgentgatewayPolicy", AccessLogPolicy)
	wait(t, done)
	first, _ := accessMode(ctx, ns)

	r.Activation.Timeout = 300 * time.Millisecond
	var ar v1.AccessRule
	_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "eng"}, &ar)
	ar.Spec.Groups = []string{"eng", "ops"}
	if err := envClient.Update(ctx, &ar); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if c := ruleReady(t, ns, "eng"); c.Reason != "RulesNotActive" {
		t.Fatalf("rule condition %+v, want the change stalled", c)
	}
	p, _ := accessLogOf(ctx, ns)
	if err := envClient.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, "access log deleted", func() error {
		if _, err := accessLogOf(ctx, ns); !apierrors.IsNotFound(err) {
			return fmt.Errorf("still there: %v", err)
		}
		return nil
	})
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	p, err := accessLogOf(ctx, ns)
	if err != nil {
		t.Fatalf("the access log was not written again during the stall: %v", err)
	}
	if a := attributes(p); a["ambit.rulesVersion"] != `"`+first["rulesVersion"]+`"` || a["ambit.accessMode"] != `"enforce"` {
		t.Errorf("attributes %v, want the published version %s", a, first["rulesVersion"])
	}
}

// A rules change where one generated object is refused by the API server (e.g. a CEL above the CRD's limit) is not
// active: the change stops at that step, the old rules version stays published, and the access log keeps it.
func TestRulesChangeWithAFailedApplyIsNotPublished(t *testing.T) {
	needEnv(t)
	ns := "ambit-gate-applyfailed"
	gateFixture(t, ns)
	ctx := t.Context()
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"},
		Activation: Activation{Timeout: 10 * time.Second, Poll: 50 * time.Millisecond}}
	done := reconcileAsync(r)
	eventually(t, "authorization written", func() error { _, err := genGet(ctx, ns, "AgentgatewayModel", "gemma"); return err })
	playAgentgateway(t, ns, "AgentgatewayModel", "gemma")
	eventually(t, "access log written", func() error { _, err := accessLogOf(ctx, ns); return err })
	playAgentgateway(t, ns, "AgentgatewayPolicy", AccessLogPolicy)
	wait(t, done)
	first, _ := accessMode(ctx, ns)

	r.Client = refusingModels{envClient}
	var ar v1.AccessRule
	_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "eng"}, &ar)
	ar.Spec.Groups = []string{"eng", "ops"}
	if err := envClient.Update(ctx, &ar); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if d, _ := accessMode(ctx, ns); d["rulesVersion"] != first["rulesVersion"] {
		t.Errorf("published %v although the model was not applied; want the old %s", d, first["rulesVersion"])
	}
	if p := pendingOf(t, ns); p["step"] != "authorization" || !strings.Contains(p["error"], "Model/gemma was not applied: spec.policies.authorization: Too long") {
		t.Errorf("pending %v", p)
	}
	if c := ruleReady(t, ns, "eng"); c.Status != metav1.ConditionFalse || c.Reason != "RulesNotActive" {
		t.Errorf("rule condition %+v", c)
	}
	if p, _ := accessLogOf(ctx, ns); attributes(p)["ambit.rulesVersion"] != `"`+first["rulesVersion"]+`"` {
		t.Errorf("the access log has the new version: %v", attributes(p))
	}
}

// refusingModels is the API server refusing every AgentgatewayModel write.
type refusingModels struct{ client.Client }

func (c refusingModels) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == "AgentgatewayModel" {
		return fmt.Errorf("spec.policies.authorization: Too long: may not be more than 16384 bytes")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}
