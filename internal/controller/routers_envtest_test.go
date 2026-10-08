package controller

import (
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestRouterReconcile(t *testing.T) {
	needEnv(t)
	ns := "ambit-router-rec"
	namespace(t, ns)
	ctx := t.Context()
	for _, o := range []client.Object{
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-k"}}},
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "all", Namespace: ns}, Spec: v1.AccessRuleSpec{AllSignedIn: true, Models: []string{"gemma", "auto"}}},
		&v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto", Namespace: ns}, Spec: v1.ModelRouterSpec{Default: "gemma"}},
	} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(AgentgatewayModelGVK)
	if err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "router-auto"}, vm); err != nil {
		t.Fatalf("virtual model not applied: %v", err)
	}
	var ar v1.AccessRule
	_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "all"}, &ar)
	if c := ar.Status.Conditions[0]; c.Status != metav1.ConditionTrue {
		t.Fatalf("rule naming the router: %s %s", c.Reason, c.Message)
	}
	var mr v1.ModelRouter
	_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "auto"}, &mr)
	if c := mr.Status.Conditions[0]; c.Reason != "Pending" { // no gateway in envtest accepts it
		t.Fatalf("router condition %s %s", c.Reason, c.Message)
	}
}

// A router that becomes invalid loses its virtual model: a stale one would keep serving old lines.
func TestInvalidRouterRemovesVirtualModel(t *testing.T) {
	needEnv(t)
	ns := "ambit-router-stale"
	namespace(t, ns)
	ctx := t.Context()
	gemma := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-k"}}}
	mr := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto", Namespace: ns}, Spec: v1.ModelRouterSpec{Default: "gemma"}}
	for _, o := range []client.Object{gemma, mr} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	_, _ = r.Reconcile(ctx, reconcile.Request{})
	// now a Model claims the router's wire name
	if err := envClient.Create(ctx, &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "clash", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "auto", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-k"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(AgentgatewayModelGVK)
	err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "router-auto"}, vm)
	if !apierrors.IsNotFound(err) && vm.GetDeletionTimestamp() == nil {
		t.Fatalf("stale virtual model kept: %v", err)
	}
	var got v1.ModelRouter
	_ = envClient.Get(ctx, client.ObjectKeyFromObject(mr), &got)
	if got.Status.Conditions[0].Reason != "NameTaken" {
		t.Fatalf("reason %s", got.Status.Conditions[0].Reason)
	}
}

func TestModelTakingRouterObjectNameSurvivesCleanup(t *testing.T) {
	needEnv(t)
	ns := "ambit-router-takeover"
	namespace(t, ns)
	ctx := t.Context()
	for _, o := range []client.Object{
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-k"}}},
		&v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto", Namespace: ns}, Spec: v1.ModelRouterSpec{Default: "gemma"}},
	} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	_, _ = r.Reconcile(ctx, reconcile.Request{})
	if err := envClient.Create(ctx, &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "router-auto", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "x/y", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-k"}}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(AgentgatewayModelGVK)
	if err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "router-auto"}, vm); err != nil {
		t.Fatalf("the Model's own object was deleted: %v", err)
	}
	if controllerOwnerKind(vm) != "Model" {
		t.Fatalf("router-auto is controlled by %s, want the Model", controllerOwnerKind(vm))
	}
}

// The usage policy exists exactly while some group has a dollar limit.
func TestUsagePolicyFollowsDollarBudgets(t *testing.T) {
	needEnv(t)
	ns := "ambit-usage-policy"
	namespace(t, ns)
	ctx := t.Context()
	b := &v1.Budget{ObjectMeta: metav1.ObjectMeta{Name: "ml", Namespace: ns}, Spec: v1.BudgetSpec{Group: "ml-research", GroupDollarsPerDay: "5"}}
	if err := envClient.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"},
		RateLimit: RateLimit{Service: "ambit-ratelimit", Port: 8081, FailureMode: "FailOpen"}}
	exists := func() bool {
		p := &unstructured.Unstructured{}
		p.SetGroupVersionKind(AgentgatewayPolicyGVK)
		err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: UsagePolicyName}, p)
		return err == nil && p.GetDeletionTimestamp() == nil
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if !exists() {
		t.Fatal("no usage policy with a dollar budget")
	}
	if err := envClient.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if exists() {
		t.Fatal("usage policy kept after the last dollar budget was removed")
	}
}
