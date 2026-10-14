package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/craftsman-apps/ambit/internal/cli/assets"
	"github.com/craftsman-apps/ambit/internal/cli/kindx"
	"github.com/craftsman-apps/ambit/internal/cli/local"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

type localFlags struct {
	name                string
	httpPort, httpsPort int
	skipChecks          bool
	timeout             time.Duration
	keyFile             string
	noKey, noPassword   bool
	model               string
	chartDir, chartRepo string
	registryConfig      string
	devArchives         []string
	devValues           []string
}

func (a *app) localCmd() *cobra.Command {
	var f localFlags
	cmd := &cobra.Command{Use: "local", Short: "A local Ambit on kind, for trying it on a laptop"}
	cmd.PersistentFlags().StringVar(&f.name, "name", "ambit", "name of the kind cluster")

	up := &cobra.Command{
		Use:   "up",
		Short: "Create or repair a local Ambit on kind, with a model, a rule and a test call",
		Args:  cobra.NoArgs,
		RunE:  run(func(cmd *cobra.Command, _ []string) error { return a.localUp(cmd.Context(), f) }),
	}
	fl := up.Flags()
	fl.IntVar(&f.httpPort, "port", 8080, "host port for the console and the gateway (HTTP)")
	fl.IntVar(&f.httpsPort, "https-port", 8443, "host port for HTTPS")
	fl.BoolVar(&f.skipChecks, "skip-checks", false, "turn preflight failures about memory into warnings")
	fl.DurationVar(&f.timeout, "timeout", 15*time.Minute, "how long to wait for the pods")
	fl.StringVar(&f.keyFile, "openrouter-key-file", "", "file that holds an OpenRouter API key (or set OPENROUTER_API_KEY)")
	fl.BoolVar(&f.noKey, "no-key", false, "use the test model; do not ask for an OpenRouter key")
	fl.StringVar(&f.model, "model", "", "OpenRouter model id to use instead of the automatic free-model choice")
	fl.BoolVar(&f.noPassword, "no-password", false, "do not print the admin password in the summary")
	fl.StringVar(&f.chartDir, "chart-dir", "", "install the charts from this directory (charts/ of a checkout) instead of the registry")
	fl.StringVar(&f.chartRepo, "chart-repo", ChartRepo, "OCI repository of the charts, for a mirror")
	fl.StringVar(&f.registryConfig, "registry-config", "", "Helm registry config for a private registry (default: Helm's)")
	addDevFlags(up, &f) // only in builds with the tag dev (ruling S9)

	down := &cobra.Command{
		Use:   "down",
		Short: "Delete the kind cluster that ambit local up created",
		Args:  cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			e, err := a.localEnv(f)
			if err != nil {
				return err
			}
			return local.Down(cmd.Context(), a.printer(), e, f.name, a.in, a.stdinTTY(), a.g.yes)
		}),
	}
	password := &cobra.Command{
		Use:   "password",
		Short: "Print the bootstrap admin password of the local Ambit",
		Args:  cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			e, err := a.localEnv(f)
			if err != nil {
				return err
			}
			return local.Password(cmd.Context(), a.printer(), e, f.name)
		}),
	}
	cmd.AddCommand(up, down, password)
	return cmd
}

func (a *app) stdinTTY() bool {
	if f, ok := a.in.(*os.File); ok {
		return term.IsTerminal(int(f.Fd()))
	}
	return false
}

func (a *app) localEnv(f localFlags) (*local.Env, error) {
	if a.testLocalEnv != nil {
		return a.testLocalEnv, nil
	}
	store, err := local.DefaultStore()
	if err != nil {
		return nil, err
	}
	logw := a.printer().Verbose()
	return &local.Env{
		Store: store, Kind: kindx.New(nilIfDiscard(logw)), ContainerID: kindx.ContainerID, Containers: kindx.Containers,
		Docker: local.ReadDockerInfo, PortFree: local.PortFree,
		OpenOps: func(kubeconfig, kctx string) (local.Ops, error) {
			return local.OpenOps(kubeconfig, kctx, logw, f.registryConfig)
		},
		HTTP: local.HTTPClient, ModelsClient: &http.Client{Timeout: 30 * time.Second}, ModelsURL: local.ModelsURL,
		CRDBundle: assets.CRDBundle(), Kubeconfig: a.g.kubeconfig,
		ModelWait: 2 * time.Minute, CallLimit: 2 * time.Minute, CallEvery: 2 * time.Second,
	}, nil
}

func (a *app) localUp(ctx context.Context, f localFlags) error {
	charts := local.Charts{Dir: f.chartDir, Repo: f.chartRepo, Version: version}
	if version == "dev" && f.chartDir == "" {
		return out.Usage("development build", "this ambit was built from source and has no chart version", "ambit local up --chart-dir <path to charts/ in a checkout>")
	}
	if f.chartDir != "" {
		for _, c := range []string{"ambit", "ambit-crds"} {
			if _, err := os.Stat(charts.Ref(c)); err != nil {
				return out.Usage("--chart-dir "+f.chartDir, "has no chart "+c, "pass the charts/ directory of a checkout")
			}
		}
		if _, err := os.Stat(charts.Ref("ambit") + "/charts"); err != nil {
			return out.Usage("--chart-dir "+f.chartDir, "the ambit chart has no dependencies in charts/ambit/charts", "helm dependency build "+charts.Ref("ambit"))
		}
	}
	if f.noKey && f.keyFile != "" {
		return out.Usage("--no-key and --openrouter-key-file", "cannot be used together", "pass one of them")
	}
	key, given, err := local.ReadKey(local.KeySource{File: f.keyFile, Env: os.Getenv("OPENROUTER_API_KEY"), NoKey: f.noKey})
	if err != nil {
		return err
	}
	e, err := a.localEnv(f)
	if err != nil {
		return err
	}
	o := local.UpOptions{Name: f.name, HTTPPort: f.httpPort, HTTPSPort: f.httpsPort, SkipChecks: f.skipChecks,
		Timeout: f.timeout, Charts: charts, Key: key, KeyGiven: given, NoKey: f.noKey, Model: f.model,
		NoPassword: f.noPassword, DevArchives: f.devArchives, DevValues: f.devValues}
	if a.stdinTTY() && !given && !f.noKey {
		o.Prompt = func() (string, error) {
			fmt.Fprint(a.out, "OpenRouter API key (Enter to use a test model): ")
			b, err := term.ReadPassword(int(a.in.(*os.File).Fd()))
			fmt.Fprintln(a.out)
			return string(b), err
		}
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	return local.Up(ctx, a.printer(), e, o)
}

// nilIfDiscard turns io.Discard into nil, so kindx uses its no-op logger.
func nilIfDiscard(w io.Writer) io.Writer {
	if w == io.Discard {
		return nil
	}
	return w
}
