package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

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
	want(t, one(t, "H1", fx{helm: &fakeHelm{err: forbidden}}), Unknown, "cannot get secrets")
	want(t, one(t, "H1", fx{helm: &fakeHelm{err: errors.New("storage driver exploded")}}), Fail, "storage driver exploded")
}

func TestH2CRDs(t *testing.T) {
	b := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: gateways.gateway.networking.k8s.io}\n"
	gw := crd("gateways.gateway.networking.k8s.io", "gateway.networking.k8s.io", true, nil, "v1")
	am := ambitCRDs()
	want(t, one(t, "H2", fx{bundle: b, ctrlObjs: append([]runtime.Object{gw}, am...)}), Pass, "1 third-party and 7 Ambit CRDs")
	want(t, one(t, "H2", fx{bundle: b, ctrlObjs: am}), Fail, "gateways.gateway.networking.k8s.io")
	want(t, one(t, "H2", fx{bundle: b, ctrlObjs: []runtime.Object{gw}}), Fail, "the ambitproject.dev CRDs")
	// Final review, Task 12 #4: some Ambit CRDs are not all of them. A half-applied ambit-crds release is a fail.
	want(t, one(t, "H2", fx{bundle: b, ctrlObjs: append([]runtime.Object{gw}, am[:3]...)}), Fail, "modelrouters.ambitproject.dev")
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

func TestH2BundleUnreadable(t *testing.T) {
	r := one(t, "H2", fx{bundle: "{{{ not yaml", ctrlObjs: ambitCRDs()})
	want(t, r, Fail, "cannot read the third-party CRD bundle")
}

// llmFx serves the gateway's /v1/chat/completions with h and points the values at it.
func llmFx(t *testing.T, h http.HandlerFunc, ms ...runtime.Object) fx {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	port := s.Listener.Addr().(*net.TCPAddr).Port
	vals := map[string]any{"console": map[string]any{"url": fmt.Sprintf("http://console.test:%d", port)}, "llm": map[string]any{"host": "127.0.0.1"}}
	return fx{deep: true, token: "tok-123", helm: &fakeHelm{all: vals}, ctrlObjs: ms}
}

func TestDeepCall(t *testing.T) {
	var gotAuth, gotModel string
	f := llmFx(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		gotModel, _ = b["model"].(string)
		w.Write([]byte(`{"ok":true}`))
	})
	e := f.env(t)
	_, all, _ := e.values()
	code, body, err := deepCall(context.Background(), e, all, "gpt-x")
	if err != nil || code != 200 || !strings.Contains(body, "ok") || gotAuth != "Bearer tok-123" || gotModel != "gpt-x" {
		t.Fatalf("= %d %q %v auth=%q model=%q", code, body, err, gotAuth, gotModel)
	}
}

func TestH8(t *testing.T) {
	ready := model("m1", "OpenAI", "ambit-llm-x", true)
	reply := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code); w.Write([]byte(body)) }
	}
	want(t, one(t, "H8", llmFx(t, reply(200, "{}"), ready)), Pass, "1 Models answered")
	want(t, one(t, "H8", llmFx(t, reply(200, "{}"), model("m2", "OpenAI", "x", false))), NA, "no Ready Model")
	for _, c := range []struct {
		code int
		sub  string
	}{
		{401, "rejected the token or a rule (HTTP 401)"},
		{403, "rejected the token or a rule (HTTP 403)"},
		{404, "model not found or not ready (HTTP 404)"},
		{429, "provider error (HTTP 429)"},
		{502, "provider error (HTTP 502)"},
	} {
		r := one(t, "H8", llmFx(t, reply(c.code, "upstream said no tok-123"), ready))
		want(t, r, Fail, c.sub)
		want(t, r, Fail, "m1 (")
		want(t, r, Fail, "upstream said no")
		if strings.Contains(r.Message, "tok-123") || strings.Contains(r.Message, "<nil>") {
			t.Fatalf("message leaks the token or prints <nil>: %s", r.Message)
		}
	}
	// transport error: the server is closed before the call
	f := llmFx(t, reply(200, "{}"), ready)
	f.helm = &fakeHelm{all: map[string]any{"console": map[string]any{"url": "http://console.test:1"}, "llm": map[string]any{"host": "127.0.0.1"}}}
	r := one(t, "H8", f)
	want(t, r, Fail, "no answer from the gateway")
	if strings.Contains(r.Message, "<nil>") {
		t.Fatalf("prints <nil>: %s", r.Message)
	}
	want(t, one(t, "H8", fx{}), NA, "--deep sends one test call per model")
}

// ambitCRDs is the full set of Established Ambit CRDs.
func ambitCRDs() []runtime.Object {
	var out []runtime.Object
	for _, n := range ambitCRDNames {
		out = append(out, crd(n, "ambitproject.dev", true, nil, "v1alpha1"))
	}
	return out
}

// ambitCRDNames must list every CRD of the ambit-crds chart; H2 fails when one is missing.
func TestAmbitCRDNamesMatchChart(t *testing.T) {
	files, err := filepath.Glob("../../../charts/ambit-crds/templates/ambit/ambitproject.dev_*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRD files found: %v", err)
	}
	var fromChart []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var v struct{ Metadata struct{ Name string } }
		if err := yaml.Unmarshal(b, &v); err != nil || v.Metadata.Name == "" {
			t.Fatalf("%s: %v", f, err)
		}
		fromChart = append(fromChart, v.Metadata.Name)
	}
	sort.Strings(fromChart)
	got := append([]string(nil), ambitCRDNames...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(fromChart, ",") {
		t.Fatalf("ambitCRDNames = %v, chart has %v", got, fromChart)
	}
}
