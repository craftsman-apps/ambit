package controller

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/craftsman-apps/ambit/internal/routerconfig"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestClassifierStateFromDeployment(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	d := func(want, avail int32) *appsv1.Deployment {
		return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Replicas: &want}, Status: appsv1.DeploymentStatus{AvailableReplicas: avail}}
	}
	for name, tc := range map[string]struct {
		d      *appsv1.Deployment
		wanted bool
		since  time.Time
		state  string
		reason string
	}{
		"off":               {d(0, 0), false, time.Time{}, "Off", ""},
		"starting":          {d(2, 0), true, now.Add(-time.Minute), "Starting", ""},
		"ready":             {d(2, 2), true, time.Time{}, "Ready", ""},
		"unhealthy":         {d(2, 1), true, time.Time{}, "Unhealthy", "ClassifierUnhealthy"},
		"start fails":       {d(2, 0), true, now.Add(-16 * time.Minute), "Starting", "ClassifierStartFailed"},
		"first time seen":   {d(2, 0), true, time.Time{}, "Starting", ""},
		"missing":           {nil, true, time.Time{}, "Off", "ClassifierNotInstalled"},
		"missing, unwanted": {nil, false, time.Time{}, "Off", ""},
	} {
		s, c := classifierState(tc.d, tc.wanted, tc.since, now)
		if s != tc.state || c.Reason != tc.reason {
			t.Errorf("%s: %s %s", name, s, c.Reason)
		}
	}
}

func TestCredentialsObject(t *testing.T) {
	cfg := routerconfig.Config{Routers: []routerconfig.Router{{Name: "auto", LLM: &routerconfig.LLM{CredentialKey: "local"}}}}
	o, err := credentialsObject("ns", cfg, map[string]string{"local": "sk-local", "unused": "sk-x"})
	if err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedStringMap(o.Object, "data")
	if o.GetKind() != "Secret" || o.GetAPIVersion() != "v1" || o.GetName() != routerconfig.SecretName || o.GetNamespace() != "ns" ||
		o.GetLabels()["app.kubernetes.io/managed-by"] != "ambit" || len(data) != 1 || data["local"] != base64.StdEncoding.EncodeToString([]byte("sk-local")) {
		t.Fatalf("%v", o.Object)
	}
}

// Only requests for a router of type LLM go to ambit-router: the condition reads the requested model, so other chat
// requests do not pay the round trip (performance row, direct chat CPU). Wire names are quoted like keywords.
// Bodies over 4,000,000 bytes skip it (no label): agentgateway v1.6.0 buffers up to 32 MiB, and ambit-router's gRPC
// server takes at most 4 MiB per message, so a larger body would fail the request with 500 once it is sent.
func TestClassifyPolicyConditionNamesRouters(t *testing.T) {
	cfg := routerconfig.Config{Hash: "h", Routers: []routerconfig.Router{{Name: "a", Wire: "auto"}, {Name: "b", Wire: `team/"x"`}}}
	cs, _, _ := unstructured.NestedSlice(classifyPolicy("ns", "ambit-llm", "ambit-router", cfg).Object, "spec", "traffic", "extProc", "conditional")
	got := cs[0].(map[string]any)["condition"]
	want := `request.path == "/v1/chat/completions" && size(request.body) <= 4000000 && json(request.body).model in ["auto", "team/\"x\""]`
	if got != want {
		t.Fatalf("condition\n got %s\nwant %s", got, want)
	}
}

// Only classifiers ambit-router can call have their Secret read: Azure and Bedrock Secrets hold other kinds of
// credentials (or none), and are never copied to ambit-router.
func TestReadKeysOnlyForOpenAICompatibleClassifiers(t *testing.T) {
	var read []string
	c := fake.NewClientBuilder().WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-local", Namespace: "ambit-system"}, Data: map[string][]byte{"Authorization": []byte("sk-local")}},
	).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Secret); ok {
			read = append(read, key.Name)
		}
		return cl.Get(ctx, key, obj, opts...)
	}}).Build()
	azure := model("gpt", "gpt-4.1")
	azure.Spec.Provider, azure.Spec.CredentialsRef = "Azure", &v1.SecretRef{Name: "ambit-llm-azure"}
	keyed := model("claude-keys", "us.anthropic.claude-sonnet-4-5-20250929-v1:0")
	keyed.Spec.Provider, keyed.Spec.CredentialsRef = "Bedrock", &v1.SecretRef{Name: "ambit-llm-aws"}
	role := model("claude-role", "anthropic.claude-sonnet-4-5-20250929-v1:0")
	role.Spec.Provider, role.Spec.CredentialsRef = "Bedrock", nil
	var routers []v1.ModelRouter
	for _, m := range []string{"gpt", "claude-keys", "claude-role", "local"} {
		r := llmRouter("auto-"+m, "coding")
		r.Spec.Classifier.LLM.Model = m
		routers = append(routers, r)
	}
	rec := &Reconciler{Live: c, Namespace: "ambit-system"}
	keys, err := rec.readKeys(t.Context(), routers, []v1.Model{azure, keyed, role, local})
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 1 || read[0] != "ambit-llm-local" || len(keys) != 1 || keys["local"] != "sk-local" {
		t.Fatalf("secrets read %v, keys %v", read, keys)
	}
}
