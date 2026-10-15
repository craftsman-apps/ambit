package cli

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/craftsman-apps/ambit/internal/cli/assets"
	"github.com/craftsman-apps/ambit/internal/cli/checks"
	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
	"github.com/craftsman-apps/ambit/internal/cli/local"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

type checkFlags struct {
	pre, local, production, noProduction, deep, failOnUnknown bool
	valuesFile                                                string
	name                                                      string
	httpPort, httpsPort                                       int
}

// selectGroups decides which groups run (spec 4.3 and 11 decision 1).
func selectGroups(f checkFlags, installed bool, consoleURL string) (map[string]bool, error) {
	if f.production && f.noProduction {
		return nil, out.Usage("--production and --no-production", "cannot be used together", "pass one of them")
	}
	g := map[string]bool{}
	if f.pre || f.local || !installed {
		g[checks.GroupPre] = true
		g[checks.GroupLocal] = f.local
		return g, nil
	}
	g[checks.GroupHealth] = true
	g[checks.GroupProduction] = f.production || (!f.noProduction && !strings.Contains(consoleURL, "localtest.me"))
	return g, nil
}

func (a *app) checkCmd() *cobra.Command {
	var f checkFlags
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check a cluster before an install, the health of an install, and the production checklist",
		Long: `Reads the cluster and prints one line per check: pass, warning (!), failure, unknown (?, the CLI could
not look) or n/a (-). It changes no object. Two checks are active: the health probes open a port-forward,
and --deep sends one test call per model (set AMBIT_TOKEN to a gateway token).
Exit code 1 when a check failed (or, with --fail-on-unknown, could not look).`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{jsonAllowed: "true"},
		RunE:        run(func(cmd *cobra.Command, _ []string) error { return a.check(cmd.Context(), f) }),
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.pre, "pre", false, "run the pre-install checks")
	fl.BoolVar(&f.local, "local", false, "with --pre: also the Docker and port checks of ambit local up")
	fl.BoolVar(&f.production, "production", false, "run the production checklist (default when console.url is not localtest.me)")
	fl.BoolVar(&f.noProduction, "no-production", false, "skip the production checklist")
	fl.BoolVar(&f.deep, "deep", false, "one test call per model, and an audit-record check (costs a few tokens; needs AMBIT_TOKEN)")
	fl.BoolVar(&f.failOnUnknown, "fail-on-unknown", false, "exit 1 when a check could not look")
	fl.StringVarP(&f.valuesFile, "values", "f", "", "values file for the pre-install checks")
	fl.StringVar(&f.name, "name", "ambit", "with --local: name of the kind cluster")
	fl.IntVar(&f.httpPort, "port", 8080, "with --local: host port for HTTP")
	fl.IntVar(&f.httpsPort, "https-port", 8443, "with --local: host port for HTTPS")
	return cmd
}

func (a *app) check(ctx context.Context, f checkFlags) error {
	p := a.printer()
	logw := p.Verbose()
	var valuesFile map[string]any
	if f.valuesFile != "" {
		b, err := os.ReadFile(f.valuesFile)
		if err != nil {
			return out.Usage("-f "+f.valuesFile, err.Error(), "check the path")
		}
		if err := yaml.Unmarshal(b, &valuesFile); err != nil {
			return out.Usage("-f "+f.valuesFile, err.Error(), "")
		}
	}
	c, connErr := kube.Connect(kube.Target{Kubeconfig: a.g.kubeconfig, Context: a.g.context, Log: logw})
	if connErr != nil && (a.g.kubeconfig != "" || a.g.context != "") {
		// The user named the target: a typo is a usage error, not a laptop without a cluster.
		return explicitTargetError(a.g.kubeconfig, a.g.context, connErr)
	}
	var results []checks.Result
	if connErr != nil {
		// No usable kubeconfig (a fresh laptop before `ambit local up`): the cluster checks cannot look, the
		// local checks still run.
		groups, err := selectGroups(f, false, "")
		if err != nil {
			return err
		}
		for _, chk := range checks.Catalogue {
			if groups[chk.Group] {
				results = append(results, checks.Result{ID: chk.ID, Group: chk.Group, Status: checks.Unknown,
					Message: "no cluster to look at: " + connErr.Error(), Next: "pass --kubeconfig and --context"})
			}
		}
		return a.finishCheck(ctx, f, groups, results, "none", "")
	}
	h, err := a.newHelm(helmx.Options{Kubeconfig: a.g.kubeconfig, Context: a.g.context, Namespace: a.g.namespace, Log: logw})
	if err != nil {
		return err
	}
	token := os.Getenv("AMBIT_TOKEN")
	p.Redactor().Add(token)
	e := &checks.Env{Kube: c.Kube, Ctrl: c.Ctrl, Helm: h, Namespace: a.g.namespace, Context: c.Context, ValuesFile: valuesFile,
		CRDBundle: assets.CRDBundle(), Deep: f.deep, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second},
		Now: time.Now,
		Forward: func(ctx context.Context, sel string, port int) (int, func(), error) {
			return kube.Forward(ctx, c, a.g.namespace, sel, port)
		},
		Exec: func(ctx context.Context, sel string, cmd []string) (string, error) {
			return kube.Exec(ctx, c, a.g.namespace, sel, cmd)
		}}

	installed, consoleURL := false, ""
	if r, err := h.Get("ambit"); err == nil && r != nil {
		installed = true
		if all, err := h.Values("ambit", true); err == nil {
			if console, ok := all["console"].(map[string]any); ok {
				consoleURL, _ = console["url"].(string)
			}
		}
	} else if err != nil {
		// A read error is not "not installed" (helmx.Get returns nil, nil for that): run the health checks, and
		// H1 reports the error.
		installed = true
	}
	groups, err := selectGroups(f, installed, consoleURL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	results = checks.Run(ctx, e, groups)
	return a.finishCheck(ctx, f, groups, results, c.Context, c.Server)
}

// finishCheck adds the local checks, prints the results and returns the exit code.
func (a *app) finishCheck(ctx context.Context, f checkFlags, groups map[string]bool, results []checks.Result, kctx, server string) error {
	p := a.printer()
	if groups[checks.GroupLocal] {
		le, err := a.localEnv(localFlags{name: f.name})
		if err != nil {
			return err
		}
		docker := le.Docker()
		var own local.Ownership
		if docker.Reachable { // kind's docker ps must not run against an unreachable daemon
			if own, err = le.Owner(ctx, f.name); err != nil {
				return err
			}
		}
		results = append(results, local.Preflight(local.PreflightInput{Name: f.name, HTTPPort: f.httpPort, HTTPSPort: f.httpsPort,
			Docker: docker, Ownership: own, PortFree: le.PortFree})...)
		if !docker.Reachable {
			results = append(results, checks.Result{ID: "L5", Group: checks.GroupLocal, Status: checks.Unknown,
				Message: "Docker is not running, so the cluster cannot be looked at", Next: "start Docker, then run ambit check --pre --local again", Source: "docker"})
		}
	}
	if a.g.output == "json" {
		if err := p.JSON(results); err != nil {
			return err
		}
	} else {
		if server == "" {
			p.Line("Checking no cluster, namespace %s", a.g.namespace)
		} else {
			p.Line("Checking context %s (%s), namespace %s", kctx, server, a.g.namespace)
		}
		checks.Print(p, results)
	}
	if code := checks.ExitCode(results, f.failOnUnknown); code != 0 {
		return exitCode(code)
	}
	return nil
}

// explicitTargetError is the usage error for a --kubeconfig or --context that cannot be used.
func explicitTargetError(kubeconfig, kctx string, connErr error) error {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	where := kubeconfig
	if where == "" {
		where = "the default kubeconfig"
	}
	if raw, err := rules.Load(); err == nil && kctx != "" && raw.Contexts[kctx] == nil {
		return out.Usage("context "+kctx+" not found in "+where, connErr.Error(), "kubectl config get-contexts")
	}
	return out.Usage("kubeconfig "+where, connErr.Error(), "check --kubeconfig and --context")
}
