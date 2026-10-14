package cli

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/craftsman-apps/ambit/internal/cli/helmx"
)

type versionInfo struct {
	CLI       string            `json:"cli"`
	Charts    string            `json:"charts"` // empty for a development build
	ChartRepo string            `json:"chartRepo"`
	Installed map[string]string `json:"installed"` // release name -> chart version; nil when unknown
	Cluster   string            `json:"cluster"`   // "reachable", "not reachable"
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "version",
		Short:       "Print the CLI version, the chart version it installs, and the installed versions",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{jsonAllowed: "true"},
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			p := a.printer()
			v := versionInfo{CLI: version, ChartRepo: ChartRepo, Cluster: "not reachable"}
			if version != "dev" {
				v.Charts = version
			}
			v.Installed, v.Cluster = a.installedVersions()
			if a.g.output == "json" {
				return p.JSON(v)
			}
			p.Line("ambit %s", v.CLI)
			if v.Charts == "" {
				p.Line("charts: none (development build; local up needs --chart-dir)")
			} else {
				p.Line("charts %s from %s", v.Charts, v.ChartRepo)
			}
			switch {
			case v.Installed == nil:
				p.Line("installed: unknown (cluster %s)", v.Cluster)
			case len(v.Installed) == 0:
				p.Line("installed: none in namespace %s", a.g.namespace)
			default:
				for _, name := range []string{"ambit-crds", "ambit"} {
					if ver, ok := v.Installed[name]; ok {
						p.Line("installed: %s %s in namespace %s", name, ver, a.g.namespace)
					}
				}
			}
			return nil
		}),
	}
}

// installedVersions reads the two releases, giving up after 3 seconds so `version` stays fast without a cluster.
func (a *app) installedVersions() (map[string]string, string) {
	type result struct {
		m   map[string]string
		err error
	}
	done := make(chan result, 1)
	go func() {
		h, err := a.newHelm(helmx.Options{Kubeconfig: a.g.kubeconfig, Context: a.g.context, Namespace: a.g.namespace})
		if err != nil {
			done <- result{err: err}
			return
		}
		m := map[string]string{}
		for _, name := range []string{"ambit-crds", "ambit"} {
			r, err := h.Get(name)
			if err != nil {
				done <- result{err: err}
				return
			}
			if r != nil {
				m[name] = r.ChartVersion
			}
		}
		done <- result{m: m}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, "not reachable"
		}
		return r.m, "reachable"
	case <-time.After(3 * time.Second):
		return nil, "not reachable"
	}
}
