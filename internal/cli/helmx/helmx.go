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

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart"
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
	Chart        string // chart name from the release metadata, e.g. "ambit"
	ChartVersion string
	Status       string // "deployed", "failed", ...
	Revision     int
}

// Client installs, upgrades and reads Helm releases in one namespace.
type Client interface {
	// Apply installs the release if it does not exist, else upgrades it. It does not wait for pods: the caller
	// waits with its own progress output. ref is a chart directory or an oci:// reference; version is ignored
	// for a directory.
	Apply(ctx context.Context, name, ref, version string, vals map[string]any) (Release, error)
	// Get returns nil, nil when the release does not exist.
	Get(name string) (*Release, error)
	// Values returns the values the user supplied (all=false, like `helm get values`) or every computed value.
	Values(name string, all bool) (map[string]any, error)
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
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(slog.NewTextHandler(out, nil)))
	if err := cfg.Init(s.RESTClientGetter(), o.Namespace, "secret"); err != nil {
		return nil, err
	}
	rc, err := registry.NewClient(registry.ClientOptCredentialsFile(s.RegistryConfig), registry.ClientOptWriter(out))
	if err != nil {
		return nil, err
	}
	cfg.RegistryClient = rc
	return &sdk{cfg: cfg, settings: s, ns: o.Namespace}, nil
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

func (h *sdk) Apply(ctx context.Context, name, ref, version string, vals map[string]any) (Release, error) {
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

// failPending marks a revision that a hard stop left in pending-install or pending-upgrade as failed. Helm refuses
// to upgrade a pending revision, but upgrades a failed one. This is the safest recovery: nothing in the cluster is
// deleted or rolled back (a rollback needs an earlier revision, which a first install lacks), and the next upgrade
// applies the chart over whatever the stopped operation left. It assumes no other operation on the release is
// running, which holds for `ambit local up` on a cluster the CLI owns.
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
	out := &Release{Name: r.Name, Revision: r.Version}
	if r.Chart != nil && r.Chart.Metadata != nil {
		out.Chart, out.ChartVersion = r.Chart.Metadata.Name, r.Chart.Metadata.Version
	}
	if r.Info != nil {
		out.Status = r.Info.Status.String()
	}
	return out
}
