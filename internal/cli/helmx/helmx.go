// Package helmx wraps the Helm SDK. It is the only package that imports Helm, so the SDK version (decided by the
// Helm 3 interoperability gate in interop_test.go) changes in one place.
package helmx

import (
	"context"
	"errors"
	"fmt"
	"io"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	rel "helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
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
	if o.RegistryConfig != "" {
		s.RegistryConfig = o.RegistryConfig
	}
	out := o.Log
	if out == nil {
		out = io.Discard
	}
	cfg := new(action.Configuration)
	if err := cfg.Init(s.RESTClientGetter(), o.Namespace, "secret", func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) }); err != nil {
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
	return toRelease(rs[len(rs)-1]), nil
}

func (h *sdk) Apply(ctx context.Context, name, ref, version string, vals map[string]any) (Release, error) {
	cur, err := h.Get(name)
	if err != nil {
		return Release{}, err
	}
	if cur == nil {
		in := action.NewInstall(h.cfg)
		in.ReleaseName, in.Namespace, in.CreateNamespace, in.Version = name, h.ns, true, version
		ch, err := h.load(ref, &in.ChartPathOptions)
		if err != nil {
			return Release{}, err
		}
		r, err := in.RunWithContext(ctx, ch, vals)
		if err != nil {
			return Release{}, err
		}
		return *toRelease(r), nil
	}
	up := action.NewUpgrade(h.cfg)
	up.Namespace, up.Version = h.ns, version
	ch, err := h.load(ref, &up.ChartPathOptions)
	if err != nil {
		return Release{}, err
	}
	r, err := up.RunWithContext(ctx, name, ch, vals)
	if err != nil {
		return Release{}, err
	}
	return *toRelease(r), nil
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

func (h *sdk) load(ref string, o *action.ChartPathOptions) (*chart.Chart, error) {
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
