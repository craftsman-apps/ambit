package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func openAIModel(ns, name, wire, url string) *v1.Model {
	return &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1.ModelSpec{Provider: "OpenAI", Model: wire, BaseURL: url, CredentialsRef: &v1.SecretRef{Name: "ambit-llm-mock"}}}
}

func TestFallbackCRDRules(t *testing.T) {
	needEnv(t)
	ns := "ambit-fallback-crd"
	namespace(t, ns)
	ctx := t.Context()
	ep := v1.ModelEndpoint{BaseURL: "http://second.svc:8000/v1", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-second"}}
	ok := openAIModel(ns, "gpt-x", "gpt-x", "http://first.svc:8000/v1")
	ok.Spec.AdditionalEndpoints = []v1.ModelEndpoint{ep}
	ok.Spec.Fallback = []string{"other"}
	if err := envClient.Create(ctx, ok); err != nil {
		t.Fatalf("valid Model refused: %v", err)
	}
	many := func(n int) []v1.ModelEndpoint {
		var out []v1.ModelEndpoint
		for range n {
			out = append(out, ep)
		}
		return out
	}
	for name, mutate := range map[string]func(*v1.ModelSpec){
		"azure-endpoint-on-openai": func(s *v1.ModelSpec) {
			s.AdditionalEndpoints = []v1.ModelEndpoint{{CredentialsRef: ep.CredentialsRef, Azure: &v1.AzureSettings{ResourceName: "eu", ResourceType: "OpenAI"}}}
		},
		"endpoint-without-credentials": func(s *v1.ModelSpec) {
			s.AdditionalEndpoints = []v1.ModelEndpoint{{BaseURL: "http://x.svc/v1"}}
		},
		"endpoint-secret-not-ambit-llm": func(s *v1.ModelSpec) {
			s.AdditionalEndpoints = []v1.ModelEndpoint{{BaseURL: "http://x.svc/v1", CredentialsRef: &v1.SecretRef{Name: "console-keys"}}}
		},
		"endpoint-bad-url": func(s *v1.ModelSpec) {
			s.AdditionalEndpoints = []v1.ModelEndpoint{{BaseURL: "not a url", CredentialsRef: ep.CredentialsRef}}
		},
		"endpoint-without-baseurl-on-self-hosted-model": func(s *v1.ModelSpec) {
			s.AdditionalEndpoints = []v1.ModelEndpoint{ep, {CredentialsRef: ep.CredentialsRef}}
		},
		"eight-endpoints":     func(s *v1.ModelSpec) { s.AdditionalEndpoints = many(8) },
		"five-fallbacks":      func(s *v1.ModelSpec) { s.Fallback = []string{"a", "b", "c", "d", "e"} },
		"duplicate-fallback":  func(s *v1.ModelSpec) { s.Fallback = []string{"a", "a"} },
		"empty-fallback-name": func(s *v1.ModelSpec) { s.Fallback = []string{""} },
		"bedrock-on-openai": func(s *v1.ModelSpec) {
			s.AdditionalEndpoints = []v1.ModelEndpoint{{CredentialsRef: ep.CredentialsRef, Bedrock: &v1.BedrockSettings{}}}
		},
		"vertexai-on-openai": func(s *v1.ModelSpec) {
			s.AdditionalEndpoints = []v1.ModelEndpoint{{CredentialsRef: ep.CredentialsRef, VertexAI: &v1.VertexAISettings{ProjectID: "acme-ml-prod"}}}
		},
	} {
		m := openAIModel(ns, name, name, "http://first.svc:8000/v1")
		mutate(&m.Spec)
		if err := envClient.Create(ctx, m); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A provider that defines its endpoint from settings: each endpoint needs them.
	az := &v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Azure", Model: "gpt-5",
		CredentialsRef: &v1.SecretRef{Name: "ambit-llm-azure"}, Azure: &v1.AzureSettings{ResourceName: "eu-west", ResourceType: "OpenAI"},
		AdditionalEndpoints: []v1.ModelEndpoint{{CredentialsRef: &v1.SecretRef{Name: "ambit-llm-azure-us"}, Azure: &v1.AzureSettings{ResourceName: "us-east", ResourceType: "OpenAI"}}}}}
	if err := envClient.Create(ctx, az); err != nil {
		t.Fatalf("Azure Model with an Azure endpoint refused: %v", err)
	}
	az.Name, az.ResourceVersion = "azure-bad", ""
	az.Spec.AdditionalEndpoints = []v1.ModelEndpoint{{CredentialsRef: &v1.SecretRef{Name: "ambit-llm-azure-us"}}}
	if err := envClient.Create(ctx, az); err == nil {
		t.Error("Azure endpoint without azure settings accepted")
	}
}

// The API server applies agentgateway's own rules to the generated objects, so this also proves that they are valid
// for v1.6.0 (a virtual model without policies, Internal concrete models with the virtual model's wire name).
func TestFallbackReconcile(t *testing.T) {
	needEnv(t)
	ns := "ambit-fallback-rec"
	namespace(t, ns)
	ctx := t.Context()
	primary := openAIModel(ns, "gpt-x", "gpt-x", "http://first.svc:8000/v1")
	primary.Spec.AdditionalEndpoints = []v1.ModelEndpoint{{BaseURL: "http://second.svc:8000/v1", CredentialsRef: &v1.SecretRef{Name: "ambit-llm-second"}}}
	primary.Spec.Fallback = []string{"backup", "missing"}
	for _, o := range []client.Object{
		primary,
		openAIModel(ns, "backup", "backup-model", "http://backup.svc:8000/v1"),
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns}, Spec: v1.AccessRuleSpec{Groups: []string{"eng", "ops"}, Models: []string{"gpt-x"}}},
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng-backup", Namespace: ns}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"backup"}}},
	} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"gpt-x", "gpt-x-ep1", "gpt-x-failover", "backup"} {
		if _, err := genGet(ctx, ns, "AgentgatewayModel", n); err != nil {
			t.Fatalf("%s not applied: %v", n, err)
		}
	}
	pol, err := genGet(ctx, ns, "AgentgatewayPolicy", RetryPolicyName)
	if err != nil {
		t.Fatalf("retry policy not applied: %v", err)
	}
	if got, _, _ := unstructured.NestedInt64(pol.Object, "spec", "traffic", "retry", "attempts"); got != 4 {
		t.Fatalf("attempts %d, want 4 (three targets)", got)
	}

	// Once agentgateway accepts the objects, the Model is Ready and its Ready message carries the notes.
	for _, n := range []string{"gpt-x", "gpt-x-ep1", "gpt-x-failover", "backup"} {
		accept(t, ns, n)
	}
	eventually(t, "notes in the Ready message", func() error {
		if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
			return err
		}
		var m v1.Model
		if err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "gpt-x"}, &m); err != nil {
			return err
		}
		c := m.Status.Conditions
		if len(c) == 0 || c[0].Status != metav1.ConditionTrue {
			return fmt.Errorf("not ready: %v", c)
		}
		for _, want := range []string{"no Model named missing", "backup is not open to ops"} {
			if !strings.Contains(c[0].Message, want) {
				return fmt.Errorf("message %q lacks %q", c[0].Message, want)
			}
		}
		return nil
	})

	// Removing the fields removes the virtual model, the extra endpoint and the retry policy, and the Model is public again.
	var m v1.Model
	if err := envClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "gpt-x"}, &m); err != nil {
		t.Fatal(err)
	}
	m.Spec.AdditionalEndpoints, m.Spec.Fallback = nil, nil
	if err := envClient.Update(ctx, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	for _, o := range [][2]string{{"AgentgatewayModel", "gpt-x-failover"}, {"AgentgatewayModel", "gpt-x-ep1"}, {"AgentgatewayPolicy", RetryPolicyName}} {
		if u, err := genGet(ctx, ns, o[0], o[1]); !apierrors.IsNotFound(err) && u.GetDeletionTimestamp() == nil {
			t.Errorf("%s %s kept (%v)", o[0], o[1], err)
		}
	}
	gx, _ := genGet(ctx, ns, "AgentgatewayModel", "gpt-x")
	if v, ok, _ := unstructured.NestedString(gx.Object, "spec", "visibility"); ok {
		t.Errorf("gpt-x is still %s", v)
	}
	if _, ok, _ := unstructured.NestedMap(gx.Object, "spec", "policies", "health"); ok {
		t.Error("gpt-x still has a health block, and backup is the only Model that needs one now: none does")
	}
}

// accept writes the status agentgateway gives an accepted AgentgatewayModel.
func accept(t *testing.T, ns, name string) {
	t.Helper()
	o, err := genGet(t.Context(), ns, "AgentgatewayModel", name)
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{
		"controllerName": "agentgateway.dev/agentgateway",
		"parentRef":      map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "ambit-llm"},
		"conditions": []any{map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted", "message": "",
			"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339), "observedGeneration": o.GetGeneration()}},
	}}, "status", "parents")
	if err := envClient.Status().Update(t.Context(), o); err != nil {
		t.Fatal(err)
	}
}
