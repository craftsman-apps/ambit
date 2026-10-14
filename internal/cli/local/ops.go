package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	ambitv1alpha1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
)

// Namespace of the local install.
const Namespace = "ambit-system"

// Charts says where the two charts come from: a checkout (Dir, development builds and CI) or an OCI repository
// at Version.
type Charts struct{ Dir, Repo, Version string }

// Ref returns the chart reference of one chart for helmx.Client.Apply.
func (c Charts) Ref(name string) string {
	if c.Dir != "" {
		return filepath.Join(c.Dir, name)
	}
	return c.Repo + "/" + name
}

// Ops are the actions local up runs inside the cluster. Every Ops talks to the context kind-<name> only.
type Ops interface {
	NodeLabels(ctx context.Context) (map[string]map[string]string, error)
	ApplyCRDs(ctx context.Context, bundle []byte) error
	InstallCharts(ctx context.Context, charts Charts, vals map[string]any, timeout time.Duration, progress func(string)) error
	SecretExists(ctx context.Context, name string) (bool, error)
	ApplySecret(ctx context.Context, name string, data map[string]string) error
	Apply(ctx context.Context, manifest []byte) error
	WaitReady(ctx context.Context, kind, name string, timeout time.Duration) error // kind: Model or AccessRule
	AdminPassword(ctx context.Context) (string, error)
}

// clusterOps is the real Ops.
type clusterOps struct {
	c *kube.Clients
	h helmx.Client
}

// OpenOps connects to kubeconfig's context kubeContext. logw receives the Kubernetes and Helm libraries' logs
// (io.Discard: off).
func OpenOps(kubeconfig, kubeContext string, logw io.Writer, registryConfig string) (Ops, error) {
	c, err := kube.Connect(kube.Target{Kubeconfig: kubeconfig, Context: kubeContext, Log: logw})
	if err != nil {
		return nil, err
	}
	h, err := helmx.New(helmx.Options{Kubeconfig: kubeconfig, Context: kubeContext, Namespace: Namespace, RegistryConfig: registryConfig, Log: logw})
	if err != nil {
		return nil, err
	}
	return &clusterOps{c: c, h: h}, nil
}

func (o *clusterOps) NodeLabels(ctx context.Context) (map[string]map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	nodes, err := o.c.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	m := map[string]map[string]string{}
	for _, n := range nodes.Items {
		m[n.Name] = n.Labels
	}
	return m, nil
}

func (o *clusterOps) ApplyCRDs(ctx context.Context, bundle []byte) error {
	if err := kube.Apply(ctx, o.c.Ctrl, "", bundle); err != nil {
		return err
	}
	names, err := kube.CRDNames(bundle)
	if err != nil {
		return err
	}
	return kube.WaitEstablished(ctx, o.c.Ctrl, names, 2*time.Minute)
}

func (o *clusterOps) InstallCharts(ctx context.Context, charts Charts, vals map[string]any, timeout time.Duration, progress func(string)) error {
	if _, err := o.h.Apply(ctx, "ambit-crds", charts.Ref("ambit-crds"), charts.Version, nil); err != nil {
		return fmt.Errorf("ambit-crds: %w", err)
	}
	if _, err := o.h.Apply(ctx, "ambit", charts.Ref("ambit"), charts.Version, vals); err != nil {
		return fmt.Errorf("ambit: %w", err)
	}
	return kube.WaitWorkloads(ctx, o.c.Kube, Namespace, []string{kube.GatewaySelector}, 10*time.Second, timeout, progress)
}

func (o *clusterOps) SecretExists(ctx context.Context, name string) (bool, error) {
	var s corev1.Secret
	err := o.c.Ctrl.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, &s)
	if err == nil {
		return true, nil
	}
	return false, client.IgnoreNotFound(err)
}

func (o *clusterOps) ApplySecret(ctx context.Context, name string, data map[string]string) error {
	m, err := yaml.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name, "labels": map[string]string{"ambitproject.dev/created-by": "ambit-cli"}}, "stringData": data})
	if err != nil {
		return err
	}
	return kube.Apply(ctx, o.c.Ctrl, Namespace, m)
}

func (o *clusterOps) Apply(ctx context.Context, manifest []byte) error {
	return kube.Apply(ctx, o.c.Ctrl, Namespace, manifest)
}

func (o *clusterOps) WaitReady(ctx context.Context, kind, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var obj client.Object
		var conds *[]metav1.Condition
		switch kind {
		case "Model":
			m := &ambitv1alpha1.Model{}
			obj, conds = m, &m.Status.Conditions
		case "AccessRule":
			a := &ambitv1alpha1.AccessRule{}
			obj, conds = a, &a.Status.Conditions
		default:
			return fmt.Errorf("WaitReady: unknown kind %s", kind)
		}
		err := o.c.Ctrl.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: name}, obj)
		if err == nil && meta.IsStatusConditionTrue(*conds, "Ready") {
			return nil
		}
		if time.Now().After(deadline) {
			msg := "no Ready condition"
			if c := meta.FindStatusCondition(*conds, "Ready"); c != nil {
				msg = c.Message
			}
			return fmt.Errorf("%s %s not Ready after %s: %s", kind, name, timeout, msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (o *clusterOps) AdminPassword(ctx context.Context) (string, error) {
	var s corev1.Secret
	if err := o.c.Ctrl.Get(ctx, client.ObjectKey{Namespace: Namespace, Name: "ambit-initial-admin"}, &s); err != nil {
		return "", err
	}
	return string(s.Data["password"]), nil
}

// CurrentContext returns the current context of the kubeconfig (empty Kubeconfig: $KUBECONFIG or ~/.kube/config).
// A missing file is not an error: the context is then empty.
func CurrentContext(kubeconfig string) (string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	if kubeconfig != "" {
		if _, err := os.Stat(kubeconfig); os.IsNotExist(err) {
			return "", nil
		}
	}
	cfg, err := rules.Load()
	if err != nil {
		return "", err
	}
	return cfg.CurrentContext, nil
}

// RestoreContext makes prev the current context again after kind made kind-<name> current. Empty prev: nothing to do.
func RestoreContext(kubeconfig, prev string) error {
	if prev == "" {
		return nil
	}
	po := clientcmd.NewDefaultPathOptions()
	po.LoadingRules.ExplicitPath = kubeconfig
	cfg, err := po.GetStartingConfig()
	if err != nil {
		return err
	}
	if cfg.CurrentContext == prev {
		return nil
	}
	cfg.CurrentContext = prev
	return clientcmd.ModifyConfig(po, *cfg, false)
}

// MergeValues returns base with each values file merged over it in order (maps merge, other values replace).
func MergeValues(base map[string]any, files []string) (map[string]any, error) {
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var v map[string]any
		if err := yaml.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		base = merge(base, v)
	}
	return base, nil
}

func merge(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}
