// Package helmx wraps the Helm SDK. It is the only package that imports Helm, so the SDK version (decided by the
// Helm 3 interoperability gate in interop_test.go) changes in one place.
package helmx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	relcommon "helm.sh/helm/v4/pkg/release/common"
	rel "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
)

// Release is what the CLI needs to know about an installed Helm release.
type Release struct {
	Name         string
	Namespace    string
	Chart        string // chart name from the release metadata, e.g. "ambit"
	ChartVersion string
	Status       string // "deployed", "failed", ...
	Revision     int
	Updated      time.Time // when the last revision started
	Description  string
}

// Client installs, upgrades and reads Helm releases in one namespace.
type Client interface {
	// Apply installs the release if it does not exist, else upgrades it. It does not wait for pods: the caller
	// waits with its own progress output. ref is a chart directory or an oci:// reference; version is ignored
	// for a directory.
	// A release left in pending-install, pending-upgrade or pending-rollback is refused with a *PendingError,
	// unless o.RecoverPending is set.
	Apply(ctx context.Context, name, ref, version string, vals map[string]any, o ApplyOptions) (Release, error)
	// Get returns nil, nil when the release does not exist.
	Get(name string) (*Release, error)
	// Values returns the values the user supplied (all=false, like `helm get values`) or every computed value.
	Values(name string, all bool) (map[string]any, error)
	// List returns the last revision of every release in the namespace, or in all namespaces (like `helm list
	// -A`, which needs list on Secrets cluster-wide).
	List(allNamespaces bool) ([]Release, error)
	// Uninstall removes a release like `helm uninstall` (no wait for the objects to go) and returns it.
	Uninstall(name string) (Release, error)
	// Render renders the chart with vals like `helm template --kube-version` (no cluster call) and returns the
	// manifest. kubeVersion is the cluster's version ("v1.31.2"); empty: Helm's default. It writes nothing.
	Render(ctx context.Context, name, ref, version string, vals map[string]any, kubeVersion string) (string, error)
}

// ApplyOptions change how Apply treats the existing release.
type ApplyOptions struct {
	// RecoverPending marks a pending last revision as failed, then upgrades over it. Only for a release that no
	// one else can be working on: `ambit local up` sets it after its ownership check (#107).
	RecoverPending bool
}

// PendingError is a release whose last revision is pending: an operation is running, or one was stopped before
// it finished. Helm keeps no other lock, so the CLI does not touch it on its own.
type PendingError struct {
	Name, Status, Description string
	Revision                  int
	Started                   time.Time
}

func (e *PendingError) Error() string {
	return fmt.Sprintf("the release %s is %s since %s (revision %d)", e.Name, e.Status, e.Started.UTC().Format(time.RFC3339), e.Revision)
}

// Options select the cluster and namespace. Log receives the SDK's log lines; nil turns them off.
type Options struct {
	Kubeconfig, Context, Namespace string
	RegistryConfig                 string // empty: Helm's default registry config
	Log                            io.Writer
}

type sdk struct {
	cfg      *action.Configuration
	settings *cli.EnvSettings
	ns       string
	// allCfg returns a configuration for every namespace (Helm reads release Secrets of all namespaces).
	allCfg func() (*action.Configuration, error)
}

// New returns a Client for the cluster and namespace in o.
func New(o Options) (Client, error) {
	s := cli.New()
	s.KubeConfig, s.KubeContext = o.Kubeconfig, o.Context
	s.SetNamespace(o.Namespace) // the kube client puts objects without a namespace here, not in the kubeconfig default
	if o.RegistryConfig != "" {
		s.RegistryConfig = o.RegistryConfig
	}
	out := o.Log
	if out == nil {
		out = io.Discard
	}
	rc, err := registry.NewClient(registry.ClientOptCredentialsFile(s.RegistryConfig), registry.ClientOptWriter(out))
	if err != nil {
		return nil, err
	}
	config := func(ns string) (*action.Configuration, error) {
		cfg := action.NewConfiguration(action.ConfigurationSetLogger(slog.NewTextHandler(out, nil)))
		if err := cfg.Init(s.RESTClientGetter(), ns, "secret"); err != nil {
			return nil, err
		}
		cfg.RegistryClient = rc
		return cfg, nil
	}
	cfg, err := config(o.Namespace)
	if err != nil {
		return nil, err
	}
	return &sdk{cfg: cfg, settings: s, ns: o.Namespace, allCfg: func() (*action.Configuration, error) { return config("") }}, nil
}

func (h *sdk) Get(name string) (*Release, error) {
	hist := action.NewHistory(h.cfg)
	hist.Max = 1
	rs, err := hist.Run(name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, nil
	}
	r, ok := rs[len(rs)-1].(*rel.Release)
	if !ok {
		return nil, fmt.Errorf("release %s: unexpected type %T", name, rs[len(rs)-1])
	}
	return toRelease(r), nil
}

func (h *sdk) Apply(ctx context.Context, name, ref, version string, vals map[string]any, o ApplyOptions) (Release, error) {
	cur, err := h.Get(name)
	if err != nil {
		return Release{}, err
	}
	if cur == nil {
		in := action.NewInstall(h.cfg)
		in.ReleaseName, in.Namespace, in.CreateNamespace, in.Version = name, h.ns, true, version
		in.ServerSideApply = false // client-side apply, as Helm 3 does, so Helm 3 can upgrade and roll back
		in.WaitStrategy = kube.HookOnlyStrategy
		ch, err := h.load(ref, &in.ChartPathOptions)
		if err != nil {
			return Release{}, err
		}
		r, err := in.RunWithContext(ctx, ch, vals)
		if err != nil {
			return Release{}, err
		}
		return *toRelease(r.(*rel.Release)), nil
	}
	if strings.HasPrefix(cur.Status, "pending-") {
		if !o.RecoverPending {
			return Release{}, h.pending(name)
		}
		if err := h.failPending(name); err != nil {
			return Release{}, err
		}
	}
	up := action.NewUpgrade(h.cfg)
	up.Namespace, up.Version = h.ns, version
	up.ServerSideApply = "false"
	up.WaitStrategy = kube.HookOnlyStrategy
	ch, err := h.load(ref, &up.ChartPathOptions)
	if err != nil {
		return Release{}, err
	}
	r, err := up.RunWithContext(ctx, name, ch, vals)
	if err != nil {
		return Release{}, err
	}
	return *toRelease(r.(*rel.Release)), nil
}

// pending describes the pending last revision of name.
func (h *sdk) pending(name string) error {
	last, err := h.cfg.Releases.Last(name)
	if err != nil {
		return err
	}
	r, ok := last.(*rel.Release)
	if !ok || r.Info == nil {
		return fmt.Errorf("release %s: unexpected type %T", name, last)
	}
	return &PendingError{Name: name, Status: r.Info.Status.String(), Description: r.Info.Description, Revision: r.Version, Started: r.Info.LastDeployed}
}

// failPending marks a revision that a hard stop left in pending-install or pending-upgrade as failed. Helm refuses
// to upgrade a pending revision, but upgrades a failed one. This is the safest recovery: nothing in the cluster is
// deleted or rolled back (a rollback needs an earlier revision, which a first install lacks), and the next upgrade
// applies the chart over whatever the stopped operation left. It assumes no other operation on the release is
// running, which holds for `ambit local up` on a cluster the CLI owns; only ApplyOptions.RecoverPending calls it.
func (h *sdk) failPending(name string) error {
	last, err := h.cfg.Releases.Last(name)
	if err != nil {
		return err
	}
	r, ok := last.(*rel.Release)
	if !ok || r.Info == nil {
		return fmt.Errorf("release %s: unexpected type %T", name, last)
	}
	r.Info.Status = relcommon.StatusFailed
	r.Info.Description = "marked failed by ambit: an earlier operation was stopped before it finished"
	return h.cfg.Releases.Update(r)
}

func (h *sdk) List(allNamespaces bool) ([]Release, error) {
	cfg := h.cfg
	if allNamespaces {
		var err error
		if cfg, err = h.allCfg(); err != nil {
			return nil, err
		}
	}
	l := action.NewList(cfg)
	l.AllNamespaces = allNamespaces
	l.SetStateMask()
	rs, err := l.Run()
	if err != nil {
		return nil, err
	}
	var out []Release
	for _, ri := range rs {
		r, ok := ri.(*rel.Release)
		if !ok {
			return nil, fmt.Errorf("unexpected release type %T", ri)
		}
		out = append(out, *toRelease(r))
	}
	return out, nil
}

func (h *sdk) Uninstall(name string) (Release, error) {
	u := action.NewUninstall(h.cfg)
	u.WaitStrategy = kube.HookOnlyStrategy
	u.DeletionPropagation = "background" // helm uninstall's default
	res, err := u.Run(name)
	if err != nil {
		return Release{}, err
	}
	r, ok := res.Release.(*rel.Release)
	if !ok {
		return Release{}, fmt.Errorf("release %s: unexpected type %T", name, res.Release)
	}
	return *toRelease(r), nil
}

func (h *sdk) Render(_ context.Context, name, ref, version string, vals map[string]any, kubeVersion string) (string, error) {
	// Client-only, like `helm template`: a server dry run cannot build objects whose CRDs are not installed yet
	// (all of Ambit's on a fresh cluster). Client-only mode replaces the configuration's client and store, so it
	// gets a configuration of its own.
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(h.cfg.Logger().Handler()))
	cfg.RegistryClient = h.cfg.RegistryClient
	in := action.NewInstall(cfg)
	in.ReleaseName, in.Namespace, in.Version = name, h.ns, version
	in.DryRunStrategy = action.DryRunClient
	if kubeVersion != "" {
		kv, err := common.ParseKubeVersion(kubeVersion)
		if err != nil {
			return "", err
		}
		in.KubeVersion = kv
	}
	ch, err := h.load(ref, &in.ChartPathOptions)
	if err != nil {
		return "", err
	}
	r, err := in.RunWithContext(context.Background(), ch, vals)
	if err != nil {
		return "", err
	}
	return r.(*rel.Release).Manifest, nil
}

func (h *sdk) Values(name string, all bool) (map[string]any, error) {
	g := action.NewGetValues(h.cfg)
	g.AllValues = all
	v, err := g.Run(name)
	if v == nil && err == nil {
		v = map[string]any{}
	}
	return v, err
}

func (h *sdk) load(ref string, o *action.ChartPathOptions) (chart.Charter, error) {
	p, err := o.LocateChart(ref, h.settings)
	if err != nil {
		return nil, err
	}
	return loader.Load(p)
}

func toRelease(r *rel.Release) *Release {
	out := &Release{Name: r.Name, Namespace: r.Namespace, Revision: r.Version}
	if r.Chart != nil && r.Chart.Metadata != nil {
		out.Chart, out.ChartVersion = r.Chart.Metadata.Name, r.Chart.Metadata.Version
	}
	if r.Info != nil {
		out.Status, out.Updated, out.Description = r.Info.Status.String(), r.Info.LastDeployed, r.Info.Description
	}
	return out
}
