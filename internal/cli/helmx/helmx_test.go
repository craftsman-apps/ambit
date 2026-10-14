package helmx

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/cli"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/registry"
	relcommon "helm.sh/helm/v4/pkg/release/common"
	rel "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
)

// memClient is a Client on Helm's in-memory release store and a fake Kubernetes client.
func memClient(t *testing.T) Client {
	t.Helper()
	rc, err := registry.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	cfg := action.NewConfiguration()
	cfg.Releases = storage.Init(driver.NewMemory())
	cfg.KubeClient = &kubefake.PrintingKubeClient{Out: io.Discard}
	cfg.Capabilities = common.DefaultCapabilities
	cfg.RegistryClient = rc
	return &sdk{cfg: cfg, settings: cli.New(), ns: "ambit-system"}
}

func TestApplyInstallsThenUpgrades(t *testing.T) {
	c := memClient(t)
	ctx := context.Background()
	if r, err := c.Get("interop"); err != nil || r != nil {
		t.Fatalf("Get before install = %v, %v; want nil, nil", r, err)
	}
	r1, err := c.Apply(ctx, "interop", "testdata/interop", "", map[string]any{"greeting": "one"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Revision != 1 || r1.Chart != "interop" || r1.ChartVersion != "0.1.0" || r1.Status != "deployed" {
		t.Fatalf("install = %+v", r1)
	}
	r2, err := c.Apply(ctx, "interop", "testdata/interop", "", map[string]any{"greeting": "two"})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Revision != 2 {
		t.Fatalf("upgrade revision = %d, want 2", r2.Revision)
	}
	got, err := c.Get("interop")
	if err != nil || got == nil || got.Revision != 2 {
		t.Fatalf("Get after upgrade = %+v, %v", got, err)
	}
}

func TestValuesUserSuppliedVersusAll(t *testing.T) {
	c := memClient(t)
	if _, err := c.Apply(context.Background(), "interop", "testdata/interop", "", map[string]any{"extra": "x"}); err != nil {
		t.Fatal(err)
	}
	user, err := c.Values("interop", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := user["greeting"]; ok || user["extra"] != "x" {
		t.Fatalf("user values = %v; want only extra", user)
	}
	all, err := c.Values("interop", true)
	if err != nil {
		t.Fatal(err)
	}
	if all["greeting"] != "hello" || all["extra"] != "x" {
		t.Fatalf("all values = %v", all)
	}
}

// The kube client puts objects that name no namespace into the namespace of the REST client getter. New must set
// it from Options, or the chart's resources land in the kubeconfig's default namespace (found by the Helm 3 gate).
func TestNewUsesNamespaceForResources(t *testing.T) {
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	const cfg = `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u, namespace: kubeconfig-default}}]
current-context: x
`
	if err := os.WriteFile(kc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{Kubeconfig: kc, Namespace: "ambit-system"})
	if err != nil {
		t.Fatal(err)
	}
	ns, _, err := c.(*sdk).settings.RESTClientGetter().ToRawKubeConfigLoader().Namespace()
	if err != nil || ns != "ambit-system" {
		t.Fatalf("namespace of the REST client getter = %q, %v; want ambit-system", ns, err)
	}
}

// Final review, Minor 1: a hard stop during the Helm step leaves the last revision pending, which Helm refuses to
// upgrade. Apply marks that revision failed (nothing is deleted) and goes on.
func TestApplyRecoversPendingRelease(t *testing.T) {
	for _, st := range []relcommon.Status{relcommon.StatusPendingInstall, relcommon.StatusPendingUpgrade} {
		t.Run(string(st), func(t *testing.T) {
			c := memClient(t)
			ctx := context.Background()
			if _, err := c.Apply(ctx, "interop", "testdata/interop", "", map[string]any{"greeting": "one"}); err != nil {
				t.Fatal(err)
			}
			s := c.(*sdk)
			last, err := s.cfg.Releases.Last("interop")
			if err != nil {
				t.Fatal(err)
			}
			r := last.(*rel.Release)
			r.Info.Status = st
			if err := s.cfg.Releases.Update(r); err != nil {
				t.Fatal(err)
			}
			got, err := c.Apply(ctx, "interop", "testdata/interop", "", map[string]any{"greeting": "two"})
			if err != nil || got.Status != "deployed" || got.Revision != 2 {
				t.Fatalf("Apply over a %s release = %+v, %v", st, got, err)
			}
		})
	}
}
