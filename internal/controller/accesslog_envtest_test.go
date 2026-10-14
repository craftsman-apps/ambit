package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func createBase(t *testing.T, ns string) *corev1.ConfigMap {
	t.Helper()
	base := baseConfigMap(testBase)
	base.Namespace, base.UID = ns, ""
	if err := envClient.Create(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	return base
}

func accessLogOf(ctx context.Context, ns string) (*unstructured.Unstructured, error) {
	return genGet(ctx, ns, "AgentgatewayPolicy", AccessLogPolicy)
}

// attributes returns the access-log attributes by name.
func attributes(p *unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	attrs, _, _ := unstructured.NestedSlice(p.Object, "spec", "frontend", "accessLog", "attributes", "add")
	for _, a := range attrs {
		m := a.(map[string]any)
		out[m["name"].(string)] = m["expression"].(string)
	}
	return out
}

func accessMode(ctx context.Context, ns string) (map[string]string, error) {
	var cm corev1.ConfigMap
	err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: AccessModeConfigMap}, &cm)
	return cm.Data, err
}

// The controller writes ambit-access-log (the chart's old name) from the base ConfigMap, in enforce mode too, and
// writes it again when it is deleted. ambit-access-mode publishes the mode and the rules version with their times.
func TestAccessLogPolicyFromTheBase(t *testing.T) {
	needEnv(t)
	ns := "ambit-accesslog"
	namespace(t, ns)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	base := createBase(t, ns)
	for _, o := range []client.Object{
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-k"}}},
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"gemma"}}},
	} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := ctrl.NewManager(envCfg.Config, ctrl.Options{Scheme: envScheme,
		Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}}, Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)}}) // TestReconcilerAgainstAPIServer runs one too
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	r := &Reconciler{Client: mgr.GetClient(), Live: mgr.GetAPIReader(), Namespace: ns, Route: RouteRef{Name: "ambit-llm"}, Now: now}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	var p *unstructured.Unstructured
	eventually(t, "access-log policy", func() error {
		p, err = accessLogOf(ctx, ns)
		return err
	})
	if p.GetLabels()["app.kubernetes.io/managed-by"] != "ambit-controller" {
		t.Errorf("labels %v", p.GetLabels())
	}
	if refs := p.GetOwnerReferences(); len(refs) != 1 || refs[0].Kind != "ConfigMap" || refs[0].UID != base.UID {
		t.Errorf("owner %v", refs)
	}
	a := attributes(p)
	if a["jwt.email"] != "jwt.email" || a["ambit.router"] != "metadata.agentgateway_user_model" || a["ambit.accessMode"] != `"enforce"` {
		t.Errorf("attributes %v", a)
	}
	var first map[string]string
	eventually(t, "ambit-access-mode", func() error {
		first, err = accessMode(ctx, ns)
		if err == nil && `"`+first["rulesVersion"]+`"` != attributes(p)["ambit.rulesVersion"] {
			p, _ = accessLogOf(ctx, ns)
			return fmt.Errorf("published %v, policy %v", first, attributes(p))
		}
		return err
	})
	if first["mode"] != "enforce" || first["modeSince"] != "2026-10-15T10:00:00Z" || first["rulesSince"] != "2026-10-15T10:00:00Z" || len(first["rulesVersion"]) != 12 {
		t.Errorf("published %v", first)
	}

	// Deleted by hand (or by Helm, upgrading from a chart that rendered it): written again.
	if err := envClient.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, "access-log policy recreated", func() error {
		q, err := accessLogOf(ctx, ns)
		if err == nil && q.GetUID() == p.GetUID() {
			return fmt.Errorf("still the deleted object")
		}
		return err
	})

	// A rules change: a new rules version since the new time; the mode keeps its time.
	clock = clock.Add(time.Hour)
	var ar v1.AccessRule
	if err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "eng"}, &ar); err != nil {
		t.Fatal(err)
	}
	ar.Spec.Groups = []string{"eng", "ops"}
	if err := envClient.Update(ctx, &ar); err != nil {
		t.Fatal(err)
	}
	eventually(t, "new rules version", func() error {
		d, err := accessMode(ctx, ns)
		if err != nil {
			return err
		}
		if d["rulesVersion"] == first["rulesVersion"] {
			return fmt.Errorf("still %v", d)
		}
		if d["rulesSince"] != "2026-10-15T11:00:00Z" || d["modeSince"] != first["modeSince"] || d["mode"] != "enforce" {
			return fmt.Errorf("published %v", d)
		}
		q, err := accessLogOf(ctx, ns)
		if err != nil || attributes(q)["ambit.rulesVersion"] != `"`+d["rulesVersion"]+`"` {
			return fmt.Errorf("policy %v (%v)", q, err)
		}
		return nil
	})
}

// A mode change sets modeSince; the policy's ambit.accessMode follows the mode.
func TestAccessModeChangeIsPublished(t *testing.T) {
	needEnv(t)
	ns := "ambit-accessmode"
	namespace(t, ns)
	ctx := t.Context()
	createBase(t, ns)
	clock := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}, Now: func() time.Time { return clock }}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	before, _ := accessMode(ctx, ns)
	clock = clock.Add(time.Hour)
	r.AccessMode = "audit"
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	after, _ := accessMode(ctx, ns)
	if after["mode"] != "audit" || after["modeSince"] != "2026-10-15T11:00:00Z" || after["rulesSince"] != before["rulesSince"] || after["rulesVersion"] != before["rulesVersion"] {
		t.Errorf("before %v, after %v", before, after)
	}
	p, err := accessLogOf(ctx, ns)
	if err != nil || attributes(p)["ambit.accessMode"] != `"audit"` {
		t.Fatalf("policy %v (%v)", p, err)
	}
	// A pass with no change writes nothing new.
	clock = clock.Add(time.Hour)
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if again, _ := accessMode(ctx, ns); again["modeSince"] != after["modeSince"] || again["rulesSince"] != after["rulesSince"] {
		t.Errorf("idle pass changed %v to %v", after, again)
	}
}

// Evidence off (no base ConfigMap): no access-log policy, as before; the mode is still published.
func TestNoAccessLogWithoutTheBase(t *testing.T) {
	needEnv(t)
	ns := "ambit-accesslog-off"
	namespace(t, ns)
	ctx := t.Context()
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := accessLogOf(ctx, ns); !apierrors.IsNotFound(err) {
		t.Fatalf("access-log policy without a base: %v", err)
	}
	if d, err := accessMode(ctx, ns); err != nil || d["mode"] != "enforce" {
		t.Fatalf("mode %v (%v)", d, err)
	}
	if err := (&AccessLogStartup{Client: envClient, Namespace: ns}).Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := accessLogOf(ctx, ns); !apierrors.IsNotFound(err) {
		t.Fatalf("startup wrote a policy without a base: %v", err)
	}
}

// At start, before it reports ready, the controller writes the policy when it is missing or still the chart's
// (an upgrade from a chart that rendered it). It does not touch a policy the controller already wrote: during a
// rolling update the leader (in the old mode) owns it.
func TestAccessLogWrittenBeforeReady(t *testing.T) {
	needEnv(t)
	ns := "ambit-accesslog-start"
	namespace(t, ns)
	ctx := t.Context()
	createBase(t, ns)
	gemma := modelWith("gemma", `jwt.groups.exists(g, g in ["eng"])`)
	gemma.SetNamespace(ns)
	gemma.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "ambit"})
	_ = unstructured.SetNestedField(gemma.Object, []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "ambit-llm"}}, "spec", "parentRefs")
	_ = unstructured.SetNestedField(gemma.Object, map[string]any{"model": "google/gemma"}, "spec", "match")
	if err := envClient.Create(ctx, gemma); err != nil {
		t.Fatal(err)
	}
	// The chart's policy, as Helm left it.
	helmOwned := policyWith(AccessLogPolicy, "traffic", "true")
	helmOwned.SetNamespace(ns)
	helmOwned.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "ambit"})
	helmOwned.Object["spec"] = map[string]any{"targetRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "ambit"}},
		"frontend": map[string]any{"accessLog": map[string]any{"attributes": map[string]any{"add": []any{map[string]any{"name": "jwt.email", "expression": "jwt.email"}}}}}}
	if err := envClient.Create(ctx, helmOwned); err != nil {
		t.Fatal(err)
	}
	s := &AccessLogStartup{Client: envClient, Namespace: ns}
	if s.Ready(nil) == nil {
		t.Fatal("ready before the policy was written")
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(nil); err != nil {
		t.Fatalf("not ready after start: %v", err)
	}
	p, err := accessLogOf(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	a := attributes(p)
	// The rules in force are the live generated objects' expressions.
	want := RulesVersion([]*unstructured.Unstructured{gemma})
	if p.GetLabels()["app.kubernetes.io/managed-by"] != "ambit-controller" || a["ambit.accessMode"] != `"enforce"` || a["ambit.rulesVersion"] != `"`+want+`"` || a["ambit.router"] == "" {
		t.Fatalf("policy %v %v", p.GetLabels(), a)
	}

	// Already the controller's: left as the leader wrote it.
	_ = unstructured.SetNestedSlice(p.Object, []any{map[string]any{"name": "ambit.accessMode", "expression": `"audit"`}}, "spec", "frontend", "accessLog", "attributes", "add")
	if err := envClient.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := (&AccessLogStartup{Client: envClient, Namespace: ns}).Start(ctx); err != nil {
		t.Fatal(err)
	}
	q, _ := accessLogOf(ctx, ns)
	if a := attributes(q); len(a) != 1 || a["ambit.accessMode"] != `"audit"` {
		t.Fatalf("startup rewrote the leader's policy: %v", a)
	}
}
