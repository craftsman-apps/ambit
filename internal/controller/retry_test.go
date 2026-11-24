package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
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

// statusFailsOnce fails the first status update of a Server, like a conflict with the console's approval would.
type statusFailsOnce struct {
	client.Client
	failed *bool
}

func (s statusFailsOnce) Status() client.SubResourceWriter {
	return statusWriter{s.Client.Status(), s.failed}
}

type statusWriter struct {
	client.SubResourceWriter
	failed *bool
}

func (w statusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*v1.Server); ok && !*w.failed {
		*w.failed = true
		return errors.New("injected conflict")
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// Issue 299, upgrade: approvals made before 0.5.0 move to the new fingerprint even when the first status write fails.
// The retry reads the tools again instead of reusing the stored list (which has no legacy fingerprints) until the next
// refresh. A changed tool and a new tool are not approved by the upgrade.
func TestUpgradeRepinSurvivesAFailedStatusWrite(t *testing.T) {
	needEnv(t)
	ns := "ambit-repin"
	namespace(t, ns)
	ctx := t.Context()
	s := &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "crm", Namespace: ns}, Spec: v1.ServerSpec{URL: "https://crm.example.com/mcp"}}
	if err := envClient.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	// The status an Ambit before 0.5.0 stored: legacy fingerprints only.
	s.Status.Tools = []v1.Tool{{Name: "get_customer", Fingerprint: "legacy-a"}, {Name: "export", Fingerprint: "legacy-b"}}
	s.Status.ApprovedTools = []v1.ToolPin{{Name: "get_customer", Fingerprint: "legacy-a"}, {Name: "export", Fingerprint: "legacy-b"}}
	if err := envClient.Status().Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	reads := 0
	failed := false
	r := &Reconciler{Client: statusFailsOnce{envClient, &failed}, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"},
		ListTools: func(context.Context, v1.Server) ([]v1.Tool, error) {
			reads++
			return []v1.Tool{
				{Name: "get_customer", Fingerprint: "new-a", LegacyFingerprint: "legacy-a"},
				{Name: "export", Fingerprint: "new-b2", LegacyFingerprint: "legacy-b2"},   // changed after approval
				{Name: "delete_all", Fingerprint: "new-c", LegacyFingerprint: "legacy-c"}, // new
			}, nil
		}}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err == nil {
		t.Fatal("the injected status failure did not reach the caller")
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if reads != 2 {
		t.Errorf("tools read %d times, want 2 (again on the retry)", reads)
	}
	var got v1.Server
	if err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "crm"}, &got); err != nil {
		t.Fatal(err)
	}
	want := []v1.ToolPin{{Name: "get_customer", Fingerprint: "new-a"}, {Name: "export", Fingerprint: "legacy-b"}}
	if !slices.Equal(got.Status.ApprovedTools, want) {
		t.Fatalf("approvals %+v, want %+v", got.Status.ApprovedTools, want)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "ToolsPinned"); c == nil || c.Reason != "ToolsChanged" || !strings.Contains(c.Message, "export, delete_all") {
		t.Fatalf("ToolsPinned %+v", c)
	}
}
