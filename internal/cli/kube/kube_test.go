package kube

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftsman-apps/ambit/internal/cli/out"
)

const kubeconfig = `apiVersion: v1
kind: Config
current-context: prod
clusters:
- {name: prod, cluster: {server: "https://prod.example.com:6443"}}
- {name: kind-ambit, cluster: {server: "https://127.0.0.1:41234"}}
contexts:
- {name: prod, context: {cluster: prod, user: u}}
- {name: kind-ambit, context: {cluster: kind-ambit, user: u}}
users:
- {name: u, user: {token: not-a-real-token}}
`

func TestConnectUsesTheNamedContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	os.WriteFile(path, []byte(kubeconfig), 0o600)
	c, err := Connect(Target{Kubeconfig: path, Context: "kind-ambit"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != "kind-ambit" || c.Server != "https://127.0.0.1:41234" {
		t.Fatalf("context %q server %q", c.Context, c.Server)
	}
	c, err = Connect(Target{Kubeconfig: path})
	if err != nil || c.Context != "prod" {
		t.Fatalf("default context = %q, %v", c.Context, err)
	}
	if _, err := Connect(Target{Kubeconfig: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("a missing explicit kubeconfig must be an error")
	}
}

func printer() (*out.Printer, *bytes.Buffer) {
	var b bytes.Buffer
	return out.New(out.Options{Out: &b, Err: &b}, &out.Redactor{}), &b
}

func TestConfirm(t *testing.T) {
	lines := []string{"context: kind-ambit", "server: https://127.0.0.1:41234"}
	p, b := printer()
	if err := Confirm(p, strings.NewReader("y\n"), true, false, lines, "Delete the kind cluster \"ambit\"?"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "context: kind-ambit\nserver: https://127.0.0.1:41234\nDelete the kind cluster \"ambit\"? [y/N]") {
		t.Fatalf("output %q", b.String())
	}
	p, _ = printer()
	if e, ok := Confirm(p, strings.NewReader("\n"), true, false, lines, "Delete?").(*out.Error); !ok || e.What != "cancelled" {
		t.Fatalf("empty answer: %v", e)
	}
	p, _ = printer()
	if e, ok := Confirm(p, strings.NewReader(""), false, false, lines, "Delete?").(*out.Error); !ok || e.Code != out.ExitUsage {
		t.Fatalf("no terminal, no --yes: %v", e)
	}
	p, _ = printer()
	if err := Confirm(p, strings.NewReader(""), false, true, lines, "Delete?"); err != nil {
		t.Fatalf("--yes: %v", err)
	}
}

func TestApplySetsNamespaceAndManager(t *testing.T) {
	// The fake client knows no REST mappings by default; a real cluster answers them from discovery.
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	c := crfake.NewClientBuilder().WithScheme(Scheme).WithRESTMapper(mapper).Build()
	m := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\ndata: {k: v}\n---\n\n---\napiVersion: v1\nkind: Secret\nmetadata: {name: ambit-llm-local}\nstringData: {Authorization: not-used}\n")
	if err := Apply(context.Background(), c, "ambit-system", m); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ambit-system", Name: "a"}, &cm); err != nil || cm.Data["k"] != "v" {
		t.Fatalf("ConfigMap: %v %v", cm.Data, err)
	}
	var s corev1.Secret
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ambit-system", Name: "ambit-llm-local"}, &s); err != nil {
		t.Fatal(err)
	}
}

func TestCRDNamesAndWaitEstablished(t *testing.T) {
	m := []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: models.ambitproject.dev}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n")
	names, err := CRDNames(m)
	if err != nil || len(names) != 1 || names[0] != "models.ambitproject.dev" {
		t.Fatalf("names %v %v", names, err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "models.ambitproject.dev"},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
			{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}}}
	c := crfake.NewClientBuilder().WithScheme(Scheme).WithObjects(crd).Build()
	if err := WaitEstablished(context.Background(), c, names, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := WaitEstablished(context.Background(), c, []string{"servers.ambitproject.dev"}, 1500*time.Millisecond); err == nil {
		t.Fatal("a missing CRD must time out")
	}
}

func readyPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
}

func deployment(name string, labels map[string]string, ready int32) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system", Labels: labels},
		Spec: appsv1.DeploymentSpec{Replicas: &one}, Status: appsv1.DeploymentStatus{ReadyReplicas: ready, UpdatedReplicas: ready}}
}

func TestWaitWorkloadsReady(t *testing.T) {
	cs := fake.NewSimpleClientset(deployment("ambit-console", nil, 1),
		deployment("ambit", map[string]string{"gateway.networking.k8s.io/gateway-name": "ambit"}, 1), readyPod("ambit-console-1"))
	err := WaitWorkloads(context.Background(), cs, "ambit-system", []string{"gateway.networking.k8s.io/gateway-name=ambit"},
		10*time.Millisecond, time.Second, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWaitWorkloadsTimeoutNamesReasons(t *testing.T) {
	pull := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ambit-console-1", Namespace: "ambit-system"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "console",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: `Back-off pulling image "ghcr.io/x"`}}}}}}
	pending := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ambit-clickhouse-0", Namespace: "ambit-system"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled,
			Status: corev1.ConditionFalse, Message: "0/1 nodes are available: 1 Insufficient memory."}}}}
	cs := fake.NewSimpleClientset(deployment("ambit-console", nil, 0), pull, pending)
	var progress []string
	err := WaitWorkloads(context.Background(), cs, "ambit-system", nil, 20*time.Millisecond, 50*time.Millisecond,
		func(l string) { progress = append(progress, l) })
	nr, ok := err.(*NotReadyError)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	all := strings.Join(nr.Pods, "\n")
	if !strings.Contains(all, `ambit-console-1: ImagePullBackOff: Back-off pulling image "ghcr.io/x"`) ||
		!strings.Contains(all, "ambit-clickhouse-0: Pending: 0/1 nodes are available: 1 Insufficient memory.") {
		t.Fatalf("pods = %q", all)
	}
	if len(progress) == 0 || progress[0] != "waiting for ambit-clickhouse-0, ambit-console-1" {
		t.Fatalf("progress = %q", progress)
	}
}

func TestPodReasonSanitizesLog(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ambit-controller-1"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "controller",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}
	got := PodReason(p, func(pod, c string) string {
		return "\x1b[31mpanic: boom\x1b[0m\nsecond line " + strings.Repeat("x", 300)
	})
	if strings.ContainsAny(got, "\x1b\n") || len([]rune(got)) > 200 || !strings.HasPrefix(got, "CrashLoopBackOff, last log line: [31mpanic: boom [0m second line") {
		t.Fatalf("reason = %q", got)
	}
}

func TestPickPod(t *testing.T) {
	notReady := readyPod("a")
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	if n, err := pickPod([]corev1.Pod{*notReady, *readyPod("b")}); err != nil || n != "b" {
		t.Fatalf("pickPod = %q, %v", n, err)
	}
	if _, err := pickPod(nil); err == nil {
		t.Fatal("no pods must be an error")
	}
}

func notReadyPods(t *testing.T, cs *fake.Clientset, required []string) (*NotReadyError, []string) {
	t.Helper()
	var progress []string
	err := WaitWorkloads(context.Background(), cs, "ambit-system", required, 10*time.Millisecond, 30*time.Millisecond,
		func(l string) { progress = append(progress, l) })
	nr, ok := err.(*NotReadyError)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	return nr, progress
}

func TestWaitWorkloadsNamesWorkloadWithoutPods(t *testing.T) {
	// A quota or admission rejection leaves a Deployment with no pods at all.
	nr, progress := notReadyPods(t, fake.NewSimpleClientset(deployment("ambit-console", nil, 0)), nil)
	all := strings.Join(nr.Pods, "\n")
	if !strings.Contains(all, "Deployment ambit-console: 0/1 ready") || strings.Contains(all, "missing") {
		t.Fatalf("pods = %q", all)
	}
	if len(progress) == 0 || progress[0] != "waiting for Deployment ambit-console: 0/1 ready" {
		t.Fatalf("progress = %q", progress)
	}
}

func TestWaitWorkloadsStatefulSetBelowReplicas(t *testing.T) {
	one := int32(1)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "ambit-clickhouse", Namespace: "ambit-system"},
		Spec: appsv1.StatefulSetSpec{Replicas: &one}}
	nr, _ := notReadyPods(t, fake.NewSimpleClientset(sts), nil)
	if all := strings.Join(nr.Pods, "\n"); !strings.Contains(all, "StatefulSet ambit-clickhouse: 0/1 ready") {
		t.Fatalf("pods = %q", all)
	}
}

func TestWaitWorkloadsSaysMissingOnlyWhenAbsent(t *testing.T) {
	sel := "gateway.networking.k8s.io/gateway-name=ambit"
	nr, _ := notReadyPods(t, fake.NewSimpleClientset(deployment("ambit-console", nil, 1)), []string{sel})
	if all := strings.Join(nr.Pods, "\n"); !strings.Contains(all, "no Deployment matches "+sel+" (missing)") {
		t.Fatalf("pods = %q", all)
	}
}

func TestWaitWorkloadsReportsAPIErrors(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", nil)
	})
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)
	})
	nr, progress := notReadyPods(t, cs, nil)
	all := strings.Join(nr.Pods, "\n")
	if !strings.Contains(all, "last API error: ") || !strings.Contains(all, "forbidden") || strings.Contains(all, "missing") {
		t.Fatalf("pods = %q", all)
	}
	if len(progress) == 0 || !strings.Contains(progress[0], "forbidden") {
		t.Fatalf("progress = %q", progress)
	}
}

// stderrDuring returns what f writes to os.Stderr.
func stderrDuring(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	f()
	os.Stderr = old
	w.Close()
	var b bytes.Buffer
	b.ReadFrom(r)
	return b.String()
}

func warningServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Warning", `299 - "v1 Foo is deprecated"`)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"kind":"PodList","apiVersion":"v1","items":[]}`))
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "config")
	cfg := strings.NewReplacer("https://prod.example.com:6443", srv.URL).Replace(kubeconfig)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConnectRoutesLibraryOutputToLog(t *testing.T) {
	path := warningServer(t)
	var log bytes.Buffer
	stderr := stderrDuring(t, func() {
		c, err := Connect(Target{Kubeconfig: path, Log: &log})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Kube.CoreV1().Pods("x").List(context.Background(), metav1.ListOptions{}); err != nil {
			t.Fatal(err)
		}
		ctrllog.Log.Info("library line")
	})
	if !strings.Contains(log.String(), "v1 Foo is deprecated") || !strings.Contains(log.String(), "library line") {
		t.Fatalf("log = %q", log.String())
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestConnectWithoutLogPrintsNothing(t *testing.T) {
	path := warningServer(t)
	stderr := stderrDuring(t, func() {
		c, err := Connect(Target{Kubeconfig: path})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Kube.CoreV1().Pods("x").List(context.Background(), metav1.ListOptions{}); err != nil {
			t.Fatal(err)
		}
		ctrllog.Log.Info("library line")
	})
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
}
