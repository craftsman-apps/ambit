package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sversion "k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftsman-apps/ambit/internal/cli/checks"
	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
)

// The target of these tests: context prod-eu on https://1.2.3.4:6443. Nothing is ever sent there: the clients
// are fakes (app.testKube).
const prodKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: prod, cluster: {server: "https://1.2.3.4:6443"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: prod-eu, context: {cluster: prod, user: u}}]
current-context: prod-eu
`

type installRig struct {
	t       *testing.T
	helm    *fakeHelm
	kube    *fake.Clientset
	ctrl    client.Client
	kc      string
	dir     string
	tty     bool
	stdin   string
	results map[string][]checks.Result // by group
	checked []map[string]bool
}

func newInstallRig(t *testing.T, objs ...runtime.Object) *installRig {
	t.Helper()
	old := version
	version = "0.5.0"
	t.Cleanup(func() { version = old })
	dir := t.TempDir()
	kc := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kc, []byte(prodKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	one := int32(1)
	ready := func(name string, labels map[string]string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system", Labels: labels},
			Spec: appsv1.DeploymentSpec{Replicas: &one}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1, UpdatedReplicas: 1}}
	}
	cs := fake.NewSimpleClientset(append([]runtime.Object{ready("ambit-controller", nil),
		ready("ambit", map[string]string{"gateway.networking.k8s.io/gateway-name": "ambit"})}, objs...)...)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &k8sversion.Info{Major: "1", Minor: "31", GitVersion: "v1.31.2"}
	r := &installRig{t: t, helm: &fakeHelm{}, kube: cs, kc: kc, dir: dir,
		ctrl:    fakeCtrl(nil),
		results: map[string][]checks.Result{checks.GroupPre: {{ID: "R1", Status: checks.Pass}}, checks.GroupProduction: {{ID: "P1", Status: checks.Pass, Message: "gateway ok"}}}}
	if err := os.WriteFile(r.path("values.yaml"), []byte("console: {url: https://console.example.com}\ndex:\n  envVars:\n    - name: SSO_SECRET\n      valueFrom: {secretKeyRef: {name: ambit-sso, key: clientSecret}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *installRig) path(name string) string { return filepath.Join(r.dir, name) }

// run runs ambit with args; stdout and stderr are one buffer.
func (r *installRig) run(args ...string) (int, string) {
	r.t.Helper()
	var b bytes.Buffer
	a := &app{in: strings.NewReader(r.stdin), out: &b, errw: &b, testTTY: r.tty,
		newHelm:  func(helmx.Options) (helmx.Client, error) { return r.helm, nil },
		testKube: &kube.Clients{Kube: r.kube, Ctrl: r.ctrl},
		testChecks: func(g map[string]bool) []checks.Result {
			r.checked = append(r.checked, g)
			for name := range g {
				return r.results[name]
			}
			return nil
		}}
	root := a.root()
	root.SetArgs(append(args, "--kubeconfig", r.kc))
	root.SetOut(&b)
	root.SetErr(&b)
	return a.exit(root.Execute()), b.String()
}

func (r *installRig) wrote() bool {
	if len(r.helm.applied) > 0 || len(r.helm.uninstalled) > 0 {
		return true
	}
	for _, a := range r.kube.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" && a.GetVerb() != "watch" && a.GetResource().Resource != "selfsubjectaccessreviews" {
			return true
		}
	}
	return false
}

func TestInstallHappyPath(t *testing.T) {
	r := newInstallRig(t)
	code, s := r.run("install", "-f", r.path("values.yaml"), "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, s)
	}
	for _, want := range []string{
		"Installing Ambit 0.5.0 into context prod-eu (https://1.2.3.4:6443), namespace ambit-system",
		"✓ Preflight", "✓ Dependency CRDs (Gateway API, agentgateway, ToolHive): 28 created", "✓ Chart ambit-crds 0.5.0", "✓ Chart ambit 0.5.0",
		"✓ Workloads ready", "✓ Ambit 0.5.0 is installed in context prod-eu, namespace ambit-system",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Join(r.helm.applied, ",") != "ambit-crds oci://ghcr.io/craftsman-apps/charts/ambit-crds 0.5.0,ambit oci://ghcr.io/craftsman-apps/charts/ambit 0.5.0" {
		t.Fatalf("applied %v", r.helm.applied)
	}
	for _, o := range r.helm.applyOpts {
		if o.RecoverPending {
			t.Fatal("install must never recover a pending release on its own (#107)")
		}
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := r.ctrl.Get(context.Background(), client.ObjectKey{Name: "gateways.gateway.networking.k8s.io"}, &crd); err != nil {
		t.Fatalf("dependency CRD not applied: %v", err)
	}
}

// 3.2 target: a context without a server URL stops with exit 1 before any question.
func TestInstallUnresolvedTargetStopsBeforeAsking(t *testing.T) {
	r := newInstallRig(t)
	os.WriteFile(r.kc, []byte(strings.Replace(prodKubeconfig, `server: "https://1.2.3.4:6443"`, `server: ""`, 1)), 0o600)
	r.tty, r.stdin = true, "y\n"
	code, s := r.run("install", "-f", r.path("values.yaml"))
	if code != 1 || !strings.Contains(s, "the target cluster cannot be resolved") || strings.Contains(s, "Install?") || r.wrote() {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

// 3.2 target and confirmation: no terminal and no --yes → exit 2; the answer n → exit 1; nothing written.
func TestInstallConfirmation(t *testing.T) {
	r := newInstallRig(t)
	code, s := r.run("install", "-f", r.path("values.yaml"))
	if code != 2 || !strings.Contains(s, "--yes") || r.wrote() {
		t.Fatalf("no terminal: exit %d\n%s", code, s)
	}
	r.tty, r.stdin = true, "n\n"
	code, s = r.run("install", "-f", r.path("values.yaml"))
	if code != 1 || !strings.Contains(s, "Install? [y/N]") || !strings.Contains(s, "cancelled") || r.wrote() {
		t.Fatalf("answer n: exit %d\n%s", code, s)
	}
}

// 3.2 refusals: an existing release (any namespace) → "use ambit upgrade"; no -f → exit 2; a dev build without
// --chart-dir → exit 2.
func TestInstallRefusals(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = []helmx.Release{{Name: "my-ambit", Namespace: "platform", Chart: "ambit", ChartVersion: "0.4.0", Status: "deployed"}}
	code, s := r.run("install", "-f", r.path("values.yaml"), "--yes")
	if code != 1 || !strings.Contains(s, "Ambit is already installed") || !strings.Contains(s, "use ambit upgrade -f values.yaml -n platform") || len(r.helm.applied) > 0 {
		t.Fatalf("existing release: exit %d\n%s", code, s)
	}
	r.helm.list = []helmx.Release{{Name: "ambit", Namespace: "ambit-system", Chart: "ambit", ChartVersion: "0.4.0", Status: "pending-upgrade"}}
	if code, s = r.run("install", "-f", r.path("values.yaml"), "--yes"); code != 1 || !strings.Contains(s, "pending-upgrade") {
		t.Fatalf("pending release: exit %d\n%s", code, s)
	}
	r.helm.list = nil
	if code, s = r.run("install", "--yes"); code != 2 || !strings.Contains(s, "needs -f values.yaml") {
		t.Fatalf("no -f: exit %d\n%s", code, s)
	}
	version = "dev"
	if code, s = r.run("install", "-f", r.path("values.yaml"), "--yes"); code != 2 || !strings.Contains(s, "development build") {
		t.Fatalf("dev build: exit %d\n%s", code, s)
	}
	if r.wrote() {
		t.Fatal("a refused install wrote something")
	}
}

// Without list on Secrets cluster-wide, install checks the target namespace only and says so.
func TestInstallListsTargetNamespaceWhenAllNamespacesForbidden(t *testing.T) {
	r := newInstallRig(t)
	r.helm.listAllErr = apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("no"))
	code, s := r.run("install", "-f", r.path("values.yaml"), "--yes")
	if code != 0 || !strings.Contains(s, "you cannot list Helm releases in all namespaces") {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

// 3.2 Argo CD-managed: the console Deployment without a Helm release → exit 1; with the tracking annotation the
// text names Argo CD.
func TestInstallRefusesAmbitManagedByAnotherTool(t *testing.T) {
	console := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ambit-console", Namespace: "ambit-system"}}
	r := newInstallRig(t, console)
	code, s := r.run("install", "-f", r.path("values.yaml"), "--yes")
	if code != 1 || !strings.Contains(s, "not as a Helm release (Argo CD or another tool manages it)") || len(r.helm.applied) > 0 {
		t.Fatalf("exit %d\n%s", code, s)
	}
	console.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "ambit:apps/Deployment:ambit-system/ambit-console"}
	r = newInstallRig(t, console)
	if code, s = r.run("install", "-f", r.path("values.yaml"), "--yes"); code != 1 || !strings.Contains(s, "(Argo CD manages it)") {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

func gatewayCRD(name, bundleVersion string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{gatewayBundleVersion: bundleVersion}},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: "gateway.networking.k8s.io", Scope: apiextensionsv1.NamespaceScoped,
			Names:    apiextensionsv1.CustomResourceDefinitionNames{Plural: strings.Split(name, ".")[0], Kind: "X"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true}}}}
}

// 3.2 CRD versions: a newer Gateway API bundle version on the cluster is left alone with a warning; an older one
// is listed and updated after the question.
func TestInstallDependencyCRDRules(t *testing.T) {
	r := newInstallRig(t)
	r.ctrl = fakeCtrl(nil, gatewayCRD("gateways.gateway.networking.k8s.io", "v1.9.0"), gatewayCRD("httproutes.gateway.networking.k8s.io", "v1.2.0"))
	r.tty, r.stdin = true, "y\nn\n"
	code, s := r.run("install", "-f", r.path("values.yaml"))
	if code != 1 || !strings.Contains(s, "left alone, newer on the cluster: gateways.gateway.networking.k8s.io (v1.9.0 on the cluster, v1.6.2 in ambit)") ||
		!strings.Contains(s, "These 1 dependency CRDs exist and differ") || !strings.Contains(s, "  httproutes.gateway.networking.k8s.io") ||
		!strings.Contains(s, "Update these 1 CRDs? [y/N]") || len(r.helm.applied) > 0 {
		t.Fatalf("answer n: exit %d\n%s", code, s)
	}
	code, s = r.run("install", "-f", r.path("values.yaml"), "--yes")
	if code != 0 || !strings.Contains(s, "26 created, 1 updated, 1 left alone (newer on the cluster)") {
		t.Fatalf("--yes: exit %d\n%s", code, s)
	}
	var gw apiextensionsv1.CustomResourceDefinition
	r.ctrl.Get(context.Background(), client.ObjectKey{Name: "gateways.gateway.networking.k8s.io"}, &gw)
	if gw.Annotations[gatewayBundleVersion] != "v1.9.0" {
		t.Fatalf("the newer Gateway API CRD was changed: %v", gw.Annotations)
	}
	var hr apiextensionsv1.CustomResourceDefinition
	r.ctrl.Get(context.Background(), client.ObjectKey{Name: "httproutes.gateway.networking.k8s.io"}, &hr)
	if hr.Annotations[gatewayBundleVersion] != "v1.6.2" {
		t.Fatalf("the older Gateway API CRD was not updated: %v", hr.Annotations)
	}
}

// 3.2 exit 3: a production check fails after a healthy install.
func TestInstallProductionFailureIsExit3(t *testing.T) {
	r := newInstallRig(t)
	r.results[checks.GroupProduction] = []checks.Result{{ID: "P3", Status: checks.Fail, Message: "no TLS"}}
	code, s := r.run("install", "-f", r.path("values.yaml"), "--yes")
	if code != 3 || !strings.Contains(s, "✗ P3  no TLS") || !strings.Contains(s, "the production checklist has failures (exit 3)") {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

// A failed pre check stops the install before anything is written.
func TestInstallPreflightFailureStops(t *testing.T) {
	r := newInstallRig(t)
	r.results[checks.GroupPre] = []checks.Result{{ID: "R2", Status: checks.Fail, Message: "you cannot create clusterroles"}}
	code, s := r.run("install", "-f", r.path("values.yaml"), "--yes")
	if code != 1 || !strings.Contains(s, "R2: you cannot create clusterroles") || r.wrote() {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

// 5.2 secret-file: the value is never in any output (--verbose on) and never in Helm values; an unknown Secret
// name is exit 2; an existing Secret is updated in the one key only, and keeps its labels.
func TestInstallSecretFile(t *testing.T) {
	const value = "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1b2c3d4e5f6g7h8i9j0kLMNOPQRSTUVWXYZabcdefghij\nklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWX==\n-----END RSA PRIVATE KEY-----\n"
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ambit-sso", Namespace: "ambit-system"}, Data: map[string][]byte{"other": []byte("keep-me")}}
	r := newInstallRig(t, existing)
	os.WriteFile(r.path("key.pem"), []byte(value), 0o600)

	code, s := r.run("install", "-f", r.path("values.yaml"), "--secret-file", "ambit-ssoo/clientSecret="+r.path("key.pem"), "--yes")
	if code != 2 || !strings.Contains(s, `the values do not name a Secret "ambit-ssoo" (names they use: ambit-sso)`) || r.wrote() {
		t.Fatalf("unknown name: exit %d\n%s", code, s)
	}
	if code, s = r.run("install", "-f", r.path("values.yaml"), "--secret-file", "ambit-sso="+r.path("key.pem"), "--yes"); code != 2 || !strings.Contains(s, "<secret>/<key>=<path>") {
		t.Fatalf("bad form: exit %d\n%s", code, s)
	}

	code, s = r.run("install", "-f", r.path("values.yaml"), "--secret-file", "ambit-sso/clientSecret="+r.path("key.pem"), "--yes", "--verbose")
	if code != 0 || !strings.Contains(s, "✓ Secrets: Secret ambit-sso key clientSecret, from "+r.path("key.pem")) {
		t.Fatalf("exit %d\n%s", code, s)
	}
	for _, l := range strings.Split(value, "\n") {
		if len(l) >= 16 && !strings.HasPrefix(l, "-----") && strings.Contains(s, l) {
			t.Fatalf("a line of the secret is in the output:\n%s", s)
		}
	}
	for _, v := range r.helm.applyVals {
		if b, _ := json.Marshal(v); strings.Contains(string(b), "MIIEowIBAAKCAQEA") {
			t.Fatal("the secret reached the Helm values")
		}
	}
	got, err := r.kube.CoreV1().Secrets("ambit-system").Get(context.Background(), "ambit-sso", metav1.GetOptions{})
	if err != nil || string(got.Data["clientSecret"]) != value || string(got.Data["other"]) != "keep-me" || got.Labels[createdByLabel] != "" {
		t.Fatalf("Secret = %+v, %v", got, err)
	}
}

// A new Secret named by gateway.tls.certificateSecret is created as a kubernetes.io/tls Secret.
func TestSecretFileCreatesTLSSecret(t *testing.T) {
	r := newInstallRig(t)
	os.WriteFile(r.path("values.yaml"), []byte("gateway: {tls: {enabled: true, certificateSecret: ambit-tls}}\n"), 0o600)
	os.WriteFile(r.path("tls.crt"), []byte("CERTIFICATE-BYTES-1234567890"), 0o600)
	os.WriteFile(r.path("tls.key"), []byte("PRIVATE-KEY-BYTES-1234567890"), 0o600)
	code, s := r.run("install", "-f", r.path("values.yaml"), "--yes",
		"--secret-file", "ambit-tls/tls.crt="+r.path("tls.crt"), "--secret-file", "ambit-tls/tls.key="+r.path("tls.key"))
	if code != 0 || strings.Contains(s, "PRIVATE-KEY-BYTES") {
		t.Fatalf("exit %d\n%s", code, s)
	}
	got, err := r.kube.CoreV1().Secrets("ambit-system").Get(context.Background(), "ambit-tls", metav1.GetOptions{})
	if err != nil || got.Type != corev1.SecretTypeTLS || len(got.Data) != 2 || got.Labels[createdByLabel] != createdByValue {
		t.Fatalf("Secret = %+v, %v", got, err)
	}
}

// 3.4 uninstall: the wrong context name → exit 1 and nothing deleted; the right one → both releases, in order, and
// the list of what stays.
func TestUninstall(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = []helmx.Release{{Name: "ambit", Chart: "ambit", ChartVersion: "0.5.0", Revision: 7, Namespace: "ambit-system"},
		{Name: "ambit-crds", Chart: "ambit-crds", ChartVersion: "0.5.0", Revision: 3, Namespace: "ambit-system"}}
	code, s := r.run("uninstall")
	if code != 2 || len(r.helm.uninstalled) > 0 {
		t.Fatalf("no terminal: exit %d\n%s", code, s)
	}
	r.tty, r.stdin = true, "y\n"
	code, s = r.run("uninstall")
	if code != 1 || !strings.Contains(s, "Type the context name (prod-eu) to uninstall:") || len(r.helm.uninstalled) > 0 {
		t.Fatalf("answer y: exit %d\n%s", code, s)
	}
	r.stdin = "prod-eu\n"
	code, s = r.run("uninstall")
	if code != 0 || strings.Join(r.helm.uninstalled, ",") != "ambit,ambit-crds" {
		t.Fatalf("exit %d, uninstalled %v\n%s", code, r.helm.uninstalled, s)
	}
	for _, want := range []string{
		"✓ Uninstalled ambit (revision 7) and ambit-crds (revision 3) from context prod-eu, namespace ambit-system",
		"CRDs and your objects: kubectl get crd -o name | grep ambitproject.dev",
		"ConfigMap ambit-access-mode", "ClickHouse volumes", "Dependency CRDs",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

// 6.1 discovery: releases with other names are found by chart; two releases of one chart → exit 1.
func TestUninstallFindsReleasesByChart(t *testing.T) {
	r := newInstallRig(t)
	r.helm.list = []helmx.Release{{Name: "platform-ambit", Chart: "ambit", Revision: 2, Namespace: "ambit-system"}, {Name: "other", Chart: "nginx"}}
	code, s := r.run("uninstall", "--yes")
	if code != 0 || strings.Join(r.helm.uninstalled, ",") != "platform-ambit" {
		t.Fatalf("exit %d, uninstalled %v\n%s", code, r.helm.uninstalled, s)
	}
	r.helm.list = append(r.helm.list, helmx.Release{Name: "ambit", Chart: "ambit"})
	r.helm.uninstalled = nil
	if code, s = r.run("uninstall", "--yes"); code != 1 || !strings.Contains(s, "more than one release of chart ambit") || len(r.helm.uninstalled) > 0 {
		t.Fatalf("two releases: exit %d\n%s", code, s)
	}
	r.helm.list = nil
	if code, s = r.run("uninstall", "--yes"); code != 1 || !strings.Contains(s, "Ambit is not installed in namespace ambit-system") {
		t.Fatalf("none: exit %d\n%s", code, s)
	}
}
