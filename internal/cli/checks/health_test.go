package checks

import (
	"errors"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	ambitv1alpha1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/cli/helmx"
)

func ok200(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("Ok.\n")) }

func TestH1Releases(t *testing.T) {
	rel := func(v, s string) *helmx.Release { return &helmx.Release{ChartVersion: v, Status: s} }
	h := func(a, b *helmx.Release) fx {
		return fx{helm: &fakeHelm{rel: map[string]*helmx.Release{"ambit-crds": a, "ambit": b}}}
	}
	want(t, one(t, "H1", h(rel("0.2.0", "deployed"), rel("0.2.0", "deployed"))), Pass, "0.2.0 are deployed")
	want(t, one(t, "H1", h(rel("0.2.0", "deployed"), rel("0.2.0", "failed"))), Fail, "ambit is failed")
	want(t, one(t, "H1", h(rel("0.1.0", "deployed"), rel("0.2.0", "deployed"))), Fail, "different versions")
	want(t, one(t, "H1", h(nil, rel("0.2.0", "deployed"))), Fail, "a release is missing")
}

func TestH2CRDs(t *testing.T) {
	b := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: gateways.gateway.networking.k8s.io}\n"
	gw := crd("gateways.gateway.networking.k8s.io", "gateway.networking.k8s.io", true, nil, "v1")
	am := crd("models.ambitproject.dev", "ambitproject.dev", true, nil, "v1alpha1")
	want(t, one(t, "H2", fx{bundle: b, ctrlObjs: []runtime.Object{gw, am}}), Pass, "1 third-party and 1 Ambit CRDs")
	want(t, one(t, "H2", fx{bundle: b, ctrlObjs: []runtime.Object{am}}), Fail, "gateways.gateway.networking.k8s.io")
	want(t, one(t, "H2", fx{bundle: b, ctrlObjs: []runtime.Object{gw}}), Fail, "the ambitproject.dev CRDs")
	want(t, one(t, "H2", fx{bundle: b, ctrlDenied: true}), Unknown, "cannot list customresourcedefinitions")
}

func TestH3Pods(t *testing.T) {
	pull := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ambit-console-1", Namespace: ns}, Status: corev1.PodStatus{Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "console", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "not found"}}}}}}
	done := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "job-1", Namespace: ns}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}
	want(t, one(t, "H3", fx{kubeObjs: []runtime.Object{readyPod("a", nil), done}}), Pass, "1 pods Ready")
	want(t, one(t, "H3", fx{kubeObjs: []runtime.Object{readyPod("a", nil), pull}}), Fail, "ambit-console-1: ImagePullBackOff: not found")
	want(t, one(t, "H3", fx{}), Fail, "no pods")
	want(t, one(t, "H3", fx{kubeDenied: true}), Unknown, "cannot list pods in ambit-system")
}

func TestProbeTarget(t *testing.T) {
	p := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Ports:          []corev1.ContainerPort{{Name: "readiness", ContainerPort: 15021}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz/ready", Port: intstr.FromString("readiness")}}},
	}}}}
	if port, path := ProbeTarget(p); port != 15021 || path != "/healthz/ready" {
		t.Fatalf("= %d %s", port, path)
	}
	if port, path := ProbeTarget(&corev1.Pod{}); port != 8080 || path != "/" {
		t.Fatalf("fallback = %d %s", port, path)
	}
}

func TestH4GatewayAndH5Dex(t *testing.T) {
	gw := readyPod("ambit-gw-1", gwLabels)
	want(t, one(t, "H4", fx{kubeObjs: []runtime.Object{gw}, forward: ok200}), Pass, "the gateway answers")
	want(t, one(t, "H4", fx{kubeObjs: []runtime.Object{gw}, forward: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }}), Fail, "HTTP 503")
	want(t, one(t, "H4", fx{kubeObjs: []runtime.Object{gw}, forwardErr: errors.New(`pods "ambit-gw-1" is forbidden: cannot create resource "pods/portforward"`)}), Unknown, "cannot create pods/portforward in ambit-system")
	want(t, one(t, "H4", fx{}), Fail, "no gateway pod")
	want(t, one(t, "H5", fx{forward: ok200}), Pass, "Dex answers")
}

func TestH6ClickHouse(t *testing.T) {
	v := func(ev map[string]any) *fakeHelm { return &fakeHelm{all: map[string]any{"evidence": ev}} }
	want(t, one(t, "H6", fx{helm: v(map[string]any{"enabled": false})}), NA, "evidence.enabled is false")
	want(t, one(t, "H6", fx{helm: v(map[string]any{"enabled": true, "clickhouse": map[string]any{"external": map[string]any{"host": "ch.example.com"}}})}), NA, "external ClickHouse ch.example.com")
	want(t, one(t, "H6", fx{helm: v(map[string]any{"enabled": true}), forward: ok200}), Pass, "ClickHouse answers")
	want(t, one(t, "H6", fx{helm: &fakeHelm{err: forbidden}}), Unknown, "cannot get secrets in ambit-system (Helm stores the release values there)")
}

func TestH7Objects(t *testing.T) {
	bad := model("broken", "OpenAI", "ambit-llm-x", false)
	bad.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "NoSecret", Message: "Secret ambit-llm-x not found", LastTransitionTime: metav1.Now()}}
	rule := &ambitv1alpha1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: ns}, Status: ambitv1alpha1.AccessRuleStatus{Conditions: readyCond()}}
	want(t, one(t, "H7", fx{ctrlObjs: []runtime.Object{model("m", "OpenAI", "ambit-llm-x", true), rule}}), Pass, "1 Models, 0 Servers and 1 AccessRules are Ready")
	want(t, one(t, "H7", fx{ctrlObjs: []runtime.Object{bad}}), Fail, "Model broken: Secret ambit-llm-x not found")
	want(t, one(t, "H7", fx{ctrlDenied: true}), Unknown, "cannot list models.ambitproject.dev in ambit-system")
}

func TestH8DeepModels(t *testing.T) {
	want(t, one(t, "H8", fx{}), NA, "--deep sends one test call per model")
	want(t, one(t, "H8", fx{deep: true}), Unknown, "set AMBIT_TOKEN")
}

func TestLLMURL(t *testing.T) {
	if u := LLMURL(map[string]any{"console": map[string]any{"url": "http://console.localtest.me:18080"}, "llm": map[string]any{"host": "llm.localtest.me"}}); u != "http://llm.localtest.me:18080" {
		t.Fatalf("local = %s", u)
	}
	if u := LLMURL(map[string]any{"console": map[string]any{"url": "https://console.example.com"}, "llm": map[string]any{"host": "llm.example.com"}}); u != "https://llm.example.com" {
		t.Fatalf("production = %s", u)
	}
}
