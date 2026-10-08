package controller

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// patchFails fails every apply of one generated kind, like a webhook or API server error would.
type patchFails struct {
	client.Client
	kind string
}

func (p patchFails) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if obj.GetObjectKind().GroupVersionKind().Kind == p.kind {
		return errors.New("injected apply failure")
	}
	return p.Client.Patch(ctx, obj, patch, opts...)
}

// A failed apply of a Model's generated object is retried on its own: without a retry, a refusal of an
// unpriced model (or any other model change) stays unapplied until some unrelated object changes.
func TestFailedApplyIsRetried(t *testing.T) {
	needEnv(t)
	ns := "ambit-retry"
	namespace(t, ns)
	ctx := t.Context()
	for _, o := range []client.Object{
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns},
			Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma-4-31b-it:free", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-openrouter"}}},
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"gemma"}}},
	} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: patchFails{Client: envClient, kind: "AgentgatewayModel"}, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	res, err := r.Reconcile(ctx, reconcile.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("a failed apply was not scheduled for a retry")
	}
	var m v1.Model
	_ = envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "gemma"}, &m)
	if readyOf(m.Status.Conditions) != "ApplyFailed" {
		t.Fatalf("Ready reason %s, want ApplyFailed", readyOf(m.Status.Conditions))
	}
}
