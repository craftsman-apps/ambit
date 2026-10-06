package controller

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/routerconfig"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func classifierSetup(t *testing.T, ns string) func() {
	t.Helper()
	needEnv(t)
	namespace(t, ns)
	ctx := t.Context()
	for _, o := range []client.Object{routerDeployment(ns),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-llm-local", Namespace: ns}, Data: map[string][]byte{"Authorization": []byte("sk-local")}},
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "gemma", Namespace: ns}, Spec: v1.ModelSpec{Provider: "Openrouter", Model: "google/gemma", CredentialsRef: v1.SecretRef{Name: "ambit-llm-k"}}},
		&v1.Model{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: ns}, Spec: v1.ModelSpec{Provider: "OpenAI", Model: "qwen", BaseURL: "http://vllm:8000/v1", CredentialsRef: v1.SecretRef{Name: "ambit-llm-local"}}},
		&v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "all", Namespace: ns}, Spec: v1.AccessRuleSpec{AllSignedIn: true, Models: []string{"gemma", "auto", "auto2"}}},
		&v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto", Namespace: ns}, Spec: v1.ModelRouterSpec{Default: "gemma", Classifier: v1.RouterClassifier{Type: "Rules"}}},
	} {
		if err := envClient.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	return func() { // a fresh Reconciler is a restarted controller: nothing is remembered between passes
		t.Helper()
		r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"}, Classifier: Classifier{Deployment: "ambit-router", Replicas: 2}}
		if _, err := r.Reconcile(ctx, reconcile.Request{}); err != nil {
			t.Fatal(err)
		}
	}
}

// Off → On → Off with a fresh reconciler each pass: the switch keeps no state.
func TestClassifierSwitch(t *testing.T) {
	ns := "ambit-classifier"
	rec := classifierSetup(t, ns)
	rec()
	if replicasOf(t, ns) != 0 || policyExists(t, ns) {
		t.Fatal("type Rules: ambit-router at 0 replicas, no ExtProc policy")
	}
	setLLM(t, ns, "auto", "local", "coding")
	rec()
	if replicasOf(t, ns) != 2 || !policyExists(t, ns) || stateOf(t, ns, "auto") != "Starting" {
		t.Fatalf("type LLM: 2 replicas, policy, Starting; got %d %v %q", replicasOf(t, ns), policyExists(t, ns), stateOf(t, ns, "auto"))
	}
	if h := policyHash(t, ns); h == "" || h != configMapHash(t, ns) {
		t.Fatalf("policy hash %q must equal the published hash %q", h, configMapHash(t, ns))
	}
	if string(secretData(t, ns)["local"]) != "sk-local" {
		t.Fatal("credentials copy")
	}
	setAvailable(t, ns, 2)
	rec()
	if stateOf(t, ns, "auto") != "Ready" {
		t.Fatalf("Ready once all replicas are available, got %q", stateOf(t, ns, "auto"))
	}
	setAvailable(t, ns, 1)
	rec()
	if stateOf(t, ns, "auto") != "Unhealthy" || reasonOf(t, ns, "auto") != "ClassifierUnhealthy" {
		t.Fatalf("Unhealthy with 1 of 2, got %q %q", stateOf(t, ns, "auto"), reasonOf(t, ns, "auto"))
	}
	setType(t, ns, "auto", "Rules")
	rec()
	if replicasOf(t, ns) != 0 || policyExists(t, ns) || stateOf(t, ns, "auto") != "" {
		t.Fatal("back to Rules: 0 replicas, no policy, no classifier state")
	}
	if _, ok := secretData(t, ns)["local"]; ok {
		t.Fatal("no key for a Model that is no longer a classifier")
	}
	rec()
	if replicasOf(t, ns) != 0 || policyExists(t, ns) {
		t.Fatal("a second pass changes nothing")
	}
}

// Deleting one LLM router while another still wants ambit-router keeps it running with the other's configuration.
func TestDeleteOneOfTwoRouters(t *testing.T) {
	ns := "ambit-classifier-two"
	rec := classifierSetup(t, ns)
	setLLM(t, ns, "auto", "local", "coding")
	createLLMRouter(t, ns, "auto2", "local", "coding")
	rec()
	before := configMapHash(t, ns)
	if err := envClient.Delete(t.Context(), &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto", Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	rec()
	if replicasOf(t, ns) != 2 || !policyExists(t, ns) || configMapHash(t, ns) == before || policyHash(t, ns) != configMapHash(t, ns) {
		t.Fatal("ambit-router stays up with a new configuration that only holds auto2")
	}
}

// A Deployment that never becomes available: ClassifierStartFailed after 15 minutes (fake clock).
func TestStartFailure(t *testing.T) {
	ns := "ambit-classifier-fail"
	classifierSetup(t, ns)
	setLLM(t, ns, "auto", "local", "coding")
	now := time.Now()
	r := &Reconciler{Client: envClient, Live: envClient, Namespace: ns, Route: RouteRef{Name: "ambit-llm"},
		Classifier: Classifier{Deployment: "ambit-router", Replicas: 2}, Now: func() time.Time { return now }}
	for _, step := range []time.Duration{0, 10 * time.Minute, 6 * time.Minute} {
		now = now.Add(step)
		if _, err := r.Reconcile(t.Context(), reconcile.Request{}); err != nil {
			t.Fatal(err)
		}
		if step == 10*time.Minute && reasonOf(t, ns, "auto") == "ClassifierStartFailed" {
			t.Fatal("not after 10 minutes")
		}
	}
	if reasonOf(t, ns, "auto") != "ClassifierStartFailed" {
		t.Fatalf("reason %s after 16 minutes", reasonOf(t, ns, "auto"))
	}
}

func TestConfigMapHasNoKey(t *testing.T) {
	ns := "ambit-classifier-key"
	rec := classifierSetup(t, ns)
	setLLM(t, ns, "auto", "local", "coding")
	rec()
	var cm corev1.ConfigMap
	if err := envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: routerconfig.ConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	var mr v1.ModelRouter
	_ = envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "auto"}, &mr)
	if strings.Contains(cm.Data[routerconfig.ConfigKey], "sk-local") || strings.Contains(fmt.Sprint(mr.Status), "sk-local") {
		t.Fatal("the key must appear only in ambit-router-credentials")
	}
}

func routerDeployment(ns string) *appsv1.Deployment {
	l := map[string]string{"app.kubernetes.io/name": "ambit-router"}
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ambit-router", Namespace: ns}, Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: l},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: l}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "router", Image: "ambit"}}}}}}
}

func replicasOf(t *testing.T, ns string) int32 {
	t.Helper()
	var d appsv1.Deployment
	if err := envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "ambit-router"}, &d); err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

func setAvailable(t *testing.T, ns string, n int32) {
	t.Helper()
	var d appsv1.Deployment
	_ = envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: "ambit-router"}, &d)
	d.Status.Replicas, d.Status.AvailableReplicas, d.Status.ReadyReplicas = *d.Spec.Replicas, n, n
	if err := envClient.Status().Update(t.Context(), &d); err != nil {
		t.Fatal(err)
	}
}

func classifyPolicyObj(t *testing.T, ns string) (*unstructured.Unstructured, bool) {
	t.Helper()
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(AgentgatewayPolicyGVK)
	err := envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: ClassifyPolicyName}, p)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return p, true
}

func policyExists(t *testing.T, ns string) bool { _, ok := classifyPolicyObj(t, ns); return ok }

func policyHash(t *testing.T, ns string) string {
	t.Helper()
	p, ok := classifyPolicyObj(t, ns)
	if !ok {
		return ""
	}
	cs, _, _ := unstructured.NestedSlice(p.Object, "spec", "traffic", "extProc", "conditional")
	a, _, _ := unstructured.NestedString(cs[0].(map[string]any), "policy", "requestAttributes", "ambit.config")
	return strings.Trim(a, "'")
}

func configMapHash(t *testing.T, ns string) string {
	t.Helper()
	var cm corev1.ConfigMap
	if err := envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: routerconfig.ConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	var c routerconfig.Config
	if err := json.Unmarshal([]byte(cm.Data[routerconfig.ConfigKey]), &c); err != nil {
		t.Fatal(err)
	}
	return c.Hash
}

func secretData(t *testing.T, ns string) map[string][]byte {
	t.Helper()
	var s corev1.Secret
	if err := envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: routerconfig.SecretName}, &s); err != nil {
		t.Fatal(err)
	}
	return s.Data
}

func llmSpec(def, model string, cats ...string) v1.ModelRouterSpec {
	s := v1.ModelRouterSpec{Default: def, Classifier: v1.RouterClassifier{Type: "LLM", LLM: &v1.RouterLLM{Model: model}}}
	for _, c := range cats {
		s.Classifier.Categories = append(s.Classifier.Categories, v1.RouterCategory{Name: c, Description: c + " prompts"})
		s.Routes = append(s.Routes, v1.RouterRoute{Model: def, Category: c})
	}
	return s
}

func setLLM(t *testing.T, ns, name, model string, cats ...string) {
	t.Helper()
	var mr v1.ModelRouter
	_ = envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, &mr)
	mr.Spec = llmSpec(mr.Spec.Default, model, cats...)
	if err := envClient.Update(t.Context(), &mr); err != nil {
		t.Fatal(err)
	}
}

func setType(t *testing.T, ns, name, typ string) {
	t.Helper()
	var mr v1.ModelRouter
	_ = envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, &mr)
	mr.Spec = v1.ModelRouterSpec{Default: mr.Spec.Default, Classifier: v1.RouterClassifier{Type: typ}}
	if err := envClient.Update(t.Context(), &mr); err != nil {
		t.Fatal(err)
	}
}

func createLLMRouter(t *testing.T, ns, name, model string, cats ...string) {
	t.Helper()
	s := llmSpec("gemma", model, cats...)
	s.Model = name
	if err := envClient.Create(t.Context(), &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: s}); err != nil {
		t.Fatal(err)
	}
}

func routerStatus(t *testing.T, ns, name string) v1.ModelRouterStatus {
	t.Helper()
	var mr v1.ModelRouter
	if err := envClient.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: name}, &mr); err != nil {
		t.Fatal(err)
	}
	return mr.Status
}

func stateOf(t *testing.T, ns, name string) string { return routerStatus(t, ns, name).ClassifierState }

func reasonOf(t *testing.T, ns, name string) string {
	c := meta.FindStatusCondition(routerStatus(t, ns, name).Conditions, "Ready")
	if c == nil {
		return ""
	}
	return c.Reason
}
