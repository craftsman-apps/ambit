package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/craftsman-apps/ambit/internal/cli/assets"
	"github.com/craftsman-apps/ambit/internal/cli/checks"
	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
	"github.com/craftsman-apps/ambit/internal/cli/local"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// installFlags are the flags of `ambit install` (spec D2 3.2).
type installFlags struct {
	values, secretFiles []string
	timeout             time.Duration
	chartDir, chartRepo string
	registryConfig      string
	dryRun              bool
}

func (a *app) installCmd() *cobra.Command {
	var f installFlags
	cmd := &cobra.Command{
		Use:   "install -f values.yaml",
		Short: "Install Ambit on a cluster: the dependency CRDs, then the Helm releases ambit-crds and ambit",
		Long: `Installs Ambit into the cluster of the kubeconfig context, as two ordinary Helm releases (ambit-crds and
ambit), so helm list, helm history and helm rollback keep working. It shows the target and asks first.
Exit code 3: Ambit is installed and healthy, but the production checklist has failures.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error { return a.install(cmd.Context(), f) }),
	}
	fl := cmd.Flags()
	fl.StringArrayVarP(&f.values, "values", "f", nil, "values file (required); may repeat: later files win, as in Helm")
	fl.StringArrayVar(&f.secretFiles, "secret-file", nil, "create or update a Secret that the values name, from a file: <secret>/<key>=<path>; may repeat")
	fl.DurationVar(&f.timeout, "timeout", 10*time.Minute, "how long to wait for the workloads")
	fl.BoolVar(&f.dryRun, "dry-run", false, "check and render everything and let the API server validate it; change nothing")
	fl.StringVar(&f.chartRepo, "chart-repo", ChartRepo, "OCI repository of the charts, for a mirror")
	fl.StringVar(&f.registryConfig, "registry-config", "", "Helm registry config for a private registry (default: Helm's)")
	addChartDirFlag(cmd, &f.chartDir)
	return cmd
}

func (a *app) uninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the Helm releases ambit and ambit-crds; CRDs, your objects and audit volumes stay",
		Long: `Uninstalls the release ambit, then ambit-crds. It never deletes CRDs or the objects people wrote. On a
terminal you confirm by typing the context name; without a terminal, --yes is required.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error { return a.uninstall(cmd.Context()) }),
	}
}

// chartsFor checks the chart source: the CLI's own version from the registry (decision CLI-4), or --chart-dir in
// a development build.
func chartsFor(cmdline, chartDir, chartRepo string) (local.Charts, error) {
	charts := local.Charts{Dir: chartDir, Repo: chartRepo, Version: version}
	if version == "dev" && chartDir == "" {
		return charts, out.Usage("development build", "this ambit was built from source and has no chart version",
			"build it with -tags dev and run "+cmdline+" --chart-dir <path to charts/ in a checkout>")
	}
	if chartDir != "" {
		for _, c := range []string{"ambit", "ambit-crds"} {
			if _, err := os.Stat(charts.Ref(c)); err != nil {
				return charts, out.Usage("--chart-dir "+chartDir, "has no chart "+c, "pass the charts/ directory of a checkout")
			}
		}
		if _, err := os.Stat(charts.Ref("ambit") + "/charts"); err != nil {
			return charts, out.Usage("--chart-dir "+chartDir, "the ambit chart has no dependencies in charts/ambit/charts", "helm dependency build "+charts.Ref("ambit"))
		}
	}
	return charts, nil
}

// chartVersion is what the output calls the charts: the CLI's version, or "from <dir>".
func chartVersion(c local.Charts) string {
	if c.Dir != "" {
		return "(charts from " + c.Dir + ")"
	}
	return c.Version
}

// target connects to the context of --kubeconfig and --context and checks that it names a cluster: a person must
// never confirm an empty or unknown target (spec D2 3.2 step 1).
func (a *app) target(registryConfig string) (*kube.Clients, helmx.Client, error) {
	logw := a.printer().Verbose()
	c, err := kube.Connect(kube.Target{Kubeconfig: a.g.kubeconfig, Context: a.g.context, Log: logw})
	if err != nil {
		if a.g.context != "" {
			if u := explicitTargetError(a.g.kubeconfig, a.g.context, err); strings.Contains(u.(*out.Error).What, "not found") {
				return nil, nil, u
			}
		}
		return nil, nil, &out.Error{What: "the target cluster cannot be resolved", Why: err.Error(), Next: "kubectl config get-contexts, then pass --context (and --kubeconfig)"}
	}
	if c.Context == "" || c.Server == "" {
		return nil, nil, &out.Error{What: "the target cluster cannot be resolved", Why: fmt.Sprintf("context %q, API server %q", c.Context, c.Server),
			Next: "kubectl config get-contexts, then pass --context (and --kubeconfig)"}
	}
	if a.testKube != nil {
		c.Kube, c.Ctrl = a.testKube.Kube, a.testKube.Ctrl
	}
	h, err := a.newHelm(helmx.Options{Kubeconfig: a.g.kubeconfig, Context: c.Context, Namespace: a.g.namespace, RegistryConfig: registryConfig, Log: logw})
	if err != nil {
		return nil, nil, err
	}
	return c, h, nil
}

func (a *app) install(ctx context.Context, f installFlags) error {
	p := a.printer()
	charts, err := chartsFor("ambit install -f values.yaml", f.chartDir, f.chartRepo)
	if err != nil {
		return err
	}
	if len(f.values) == 0 {
		return out.Usage("no values file", "ambit install needs -f values.yaml (ambit init writes one)", "ambit install -f values.yaml")
	}
	vals, err := local.MergeValues(map[string]any{}, f.values)
	if err != nil {
		return out.Usage("-f", err.Error(), "check the values files")
	}
	secrets, err := readSecretFiles(f.secretFiles, vals, p.Redactor())
	if err != nil {
		return err
	}
	c, h, err := a.target(f.registryConfig)
	if err != nil {
		return err
	}
	ns := a.g.namespace

	// 1. Target and question (no question for a dry run: it writes nothing).
	targetLine := fmt.Sprintf("Installing Ambit %s into context %s (%s), namespace %s", chartVersion(charts), c.Context, c.Server, ns)
	if f.dryRun {
		p.Line("%s (dry run)", targetLine)
	} else if err := kube.Confirm(p, a.in, a.stdinTTY(), a.g.yes, []string{targetLine}, "Install?"); err != nil {
		return err
	}
	step := stepper(p)

	// 2. Preflight: releases, Argo CD, the pre checks, the charts render.
	var plan *crdPlan
	if err := step("Preflight", func() error {
		if err := refuseInstalled(ctx, p, c, h, ns); err != nil {
			return err
		}
		if err := a.preChecks(ctx, p, c, h, vals, secrets); err != nil {
			return err
		}
		if err := renderBoth(ctx, c, h, charts, vals); err != nil {
			return err
		}
		plan, err = planCRDs(ctx, c.Ctrl, assets.CRDBundle())
		return err
	}); err != nil {
		return err
	}
	if f.dryRun {
		return a.dryRun(ctx, p, c, h, charts, vals, plan, secrets)
	}
	update, err := a.askCRDUpdates(p, plan)
	if err != nil {
		return err
	}

	// 3. Secrets from --secret-file, written outside Helm.
	if len(secrets) > 0 {
		if err := step("Secrets: "+strings.Join(describeSecretFiles(secrets), "; "), func() error {
			return writeSecretFiles(ctx, c.Kube, ns, secrets, vals)
		}); err != nil {
			return &out.Error{What: "the Secrets were not written", Why: err.Error(), Next: "check that you can create Secrets in " + ns + ", then run ambit install again"}
		}
	}

	// 4-7. Dependency CRDs, charts, wait, production checks.
	return a.applyAndCheck(ctx, p, c, h, charts, vals, plan, update, f.timeout, "installed", "ambit install")
}

// stepper prints each step as one line with its duration, like local up.
func stepper(p *out.Printer) func(string, func() error) error {
	return func(what string, f func() error) error {
		start := time.Now()
		err := f()
		p.Step(err == nil, what, time.Since(start))
		return err
	}
}

// applyAndCheck is steps 4 to 7 of install and upgrade.
func (a *app) applyAndCheck(ctx context.Context, p *out.Printer, c *kube.Clients, h helmx.Client, charts local.Charts, vals map[string]any,
	plan *crdPlan, update bool, timeout time.Duration, done, again string) error {
	step := stepper(p)
	ns := a.g.namespace
	if err := step("Dependency CRDs (Gateway API, agentgateway, ToolHive): "+plan.summary(update), func() error {
		return plan.apply(ctx, c.Ctrl, update, false)
	}); err != nil {
		return &out.Error{What: "the dependency CRDs were not applied", Why: err.Error(), Next: "run " + again + " again (add --verbose for details)"}
	}
	for _, rel := range []struct {
		name string
		vals map[string]any
	}{{"ambit-crds", nil}, {"ambit", vals}} {
		var r helmx.Release
		if err := step("Chart "+rel.name+" "+chartVersion(charts), func() error {
			var err error
			r, err = h.Apply(ctx, rel.name, charts.Ref(rel.name), charts.Version, rel.vals, helmx.ApplyOptions{})
			return err
		}); err != nil {
			var pe *helmx.PendingError
			if errors.As(err, &pe) {
				return pendingRefusal(pe, ns)
			}
			return &out.Error{What: "the release " + rel.name + " failed", Why: err.Error(),
				Next: fmt.Sprintf("helm history %s -n %s shows the revisions; fix the cause, then run %s again", rel.name, ns, again)}
		}
		p.Line("  release %s revision %d, %s", r.Name, r.Revision, r.Status)
	}
	if err := step("Workloads ready", func() error {
		return kube.WaitWorkloads(ctx, c.Kube, ns, []string{kube.GatewaySelector}, 10*time.Second, timeout, func(l string) { p.Line("  %s", l) })
	}); err != nil {
		var nr *kube.NotReadyError
		if errors.As(err, &nr) {
			p.Line("  not ready after %s:", nr.Timeout)
			for _, l := range nr.Pods {
				p.Line("    %s", l)
			}
			return &out.Error{What: "the workloads did not become ready", Why: fmt.Sprintf("%d not ready after %s", len(nr.Pods), nr.Timeout),
				Next: fmt.Sprintf("kubectl -n %s get pods; helm rollback ambit <revision> -n %s goes back", ns, ns)}
		}
		return err
	}
	p.Line("Production checklist:")
	results := a.runChecks(ctx, a.checksEnv(c, h, nil), map[string]bool{checks.GroupProduction: true})
	checks.Print(p, results)
	if checks.ExitCode(results, false) != 0 {
		p.Line("%s Ambit %s is %s in context %s, namespace %s, and healthy; the production checklist has failures (exit 3)", p.Mark("warn"), chartVersion(charts), done, c.Context, ns)
		return exitCode(out.ExitProductionFailed)
	}
	p.Line("%s Ambit %s is %s in context %s, namespace %s", p.Mark("ok"), chartVersion(charts), done, c.Context, ns)
	return nil
}

// runChecks runs check groups; a variable on app so a test can give fixed results.
func (a *app) runChecks(ctx context.Context, e *checks.Env, groups map[string]bool) []checks.Result {
	if a.testChecks != nil {
		return a.testChecks(groups)
	}
	return checks.Run(ctx, e, groups)
}

// checksEnv is the check catalogue's view of the target, as `ambit check` builds it.
func (a *app) checksEnv(c *kube.Clients, h helmx.Client, valuesFile map[string]any) *checks.Env {
	return &checks.Env{Kube: c.Kube, Ctrl: c.Ctrl, Helm: h, Namespace: a.g.namespace, Context: c.Context, ValuesFile: valuesFile,
		CRDBundle: assets.CRDBundle(), HTTP: newCheckHTTP(), Now: time.Now,
		Forward: func(ctx context.Context, sel string, port int) (int, func(), error) {
			return kube.Forward(ctx, c, a.g.namespace, sel, port)
		},
		Exec: func(ctx context.Context, sel string, cmd []string) (string, error) {
			return kube.Exec(ctx, c, a.g.namespace, sel, cmd)
		}}
}

// refuseInstalled stops install when Ambit is installed: a Helm release of chart ambit or ambit-crds in any
// namespace, or the console Deployment without a Helm release (another tool manages Ambit; spec D2 6.1).
func refuseInstalled(ctx context.Context, p *out.Printer, c *kube.Clients, h helmx.Client, ns string) error {
	rs, err := h.List(true)
	if err != nil {
		if !denied(err) {
			return &out.Error{What: "the Helm releases cannot be read", Why: err.Error(), Next: "check your access to the cluster (add --verbose for details)"}
		}
		p.Line("  ! you cannot list Helm releases in all namespaces (list on Secrets cluster-wide); checked namespace %s only", ns)
		if rs, err = h.List(false); err != nil {
			return &out.Error{What: "the Helm releases in " + ns + " cannot be read", Why: err.Error(), Next: "ask for get and list on Secrets in " + ns}
		}
	}
	for _, r := range rs {
		if r.Chart != "ambit" && r.Chart != "ambit-crds" {
			continue
		}
		next := "ambit upgrade -f values.yaml"
		if r.Namespace != ns {
			next += " -n " + r.Namespace
		}
		why := fmt.Sprintf("the Helm release %s (chart %s %s, %s) is in namespace %s", r.Name, r.Chart, r.ChartVersion, r.Status, r.Namespace)
		if strings.HasPrefix(r.Status, "pending-") {
			why += "; an install or upgrade may be running"
		}
		return &out.Error{What: "Ambit is already installed", Why: why, Next: "use " + next}
	}
	return refuseOtherTool(ctx, c, ns, rs)
}

// refuseOtherTool: the console Deployment exists, but no Helm release of chart ambit is in the namespace. Argo CD
// renders the chart itself and keeps no Helm release, so the objects are the only sign.
func refuseOtherTool(ctx context.Context, c *kube.Clients, ns string, rs []helmx.Release) error {
	for _, r := range rs {
		if r.Chart == "ambit" && r.Namespace == ns {
			return nil
		}
	}
	d, err := c.Kube.AppsV1().Deployments(ns).Get(ctx, "ambit-console", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return &out.Error{What: "the Deployment ambit-console cannot be read", Why: err.Error(), Next: "ask for get on Deployments in " + ns}
	}
	return otherToolError(d, ns)
}

func otherToolError(d *appsv1.Deployment, ns string) error {
	tool := "Argo CD or another tool"
	if d.Annotations["argocd.argoproj.io/tracking-id"] != "" || d.Labels["argocd.argoproj.io/instance"] != "" {
		tool = "Argo CD"
	}
	return &out.Error{What: "Ambit is installed here, but not as a Helm release (" + tool + " manages it); change it there",
		Why:  "the Deployment ambit-console exists in " + ns + " and no Helm release of chart ambit is there",
		Next: "change the install with " + tool + ", or see https://ambitproject.dev/docs/install#argo-cd-and-gitops"}
}

func denied(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || strings.Contains(err.Error(), "forbidden")
}

// preChecks runs the pre-install checks that apply to install: R1 (Kubernetes version), R2 (permissions), R4
// (StorageClass) and R5 (TLS Secret; skipped when --secret-file writes it). R3 and R6 are what install itself
// handles. A failure stops the install.
func (a *app) preChecks(ctx context.Context, p *out.Printer, c *kube.Clients, h helmx.Client, vals map[string]any, secrets []secretFile) error {
	tls, _ := lookup(vals, "gateway.tls.certificateSecret").(string)
	fromFile := false
	for _, s := range secrets {
		fromFile = fromFile || s.Secret == tls
	}
	var failed []string
	for _, r := range a.runChecks(ctx, a.checksEnv(c, h, vals), map[string]bool{checks.GroupPre: true}) {
		if r.ID == "R3" || r.ID == "R6" || (r.ID == "R5" && fromFile) || r.Status == checks.Pass || r.Status == checks.NA {
			continue
		}
		p.Line("  %s %s %s", map[checks.Status]string{checks.Warn: p.Mark("warn"), checks.Fail: p.Mark("fail"), checks.Unknown: "?"}[r.Status], r.ID, r.Message)
		if r.Status == checks.Fail {
			failed = append(failed, r.ID+": "+r.Message)
			if r.Next != "" {
				p.Line("      next: %s", r.Next)
			}
		}
	}
	if len(failed) > 0 {
		return &out.Error{What: "preflight", Why: strings.Join(failed, "; "), Next: "fix the failed checks above, then run the command again"}
	}
	return nil
}

// renderBoth renders both charts with the values, client-only, for the cluster's Kubernetes version.
func renderBoth(ctx context.Context, c *kube.Clients, h helmx.Client, charts local.Charts, vals map[string]any) error {
	kv := ""
	if v, err := c.Kube.Discovery().ServerVersion(); err == nil {
		kv = v.GitVersion
	}
	for _, rel := range []struct {
		name string
		vals map[string]any
	}{{"ambit-crds", nil}, {"ambit", vals}} {
		if _, err := h.Render(ctx, rel.name, charts.Ref(rel.name), charts.Version, rel.vals, kv); err != nil {
			return &out.Error{What: "the chart " + rel.name + " does not render with these values", Why: err.Error(), Next: "fix the values file; the chart's values.yaml lists every setting"}
		}
	}
	return nil
}

// askCRDUpdates prints the CRD plan and asks before changing CRDs that exist (spec D2 3.2 step 4). It reports
// whether to update them.
func (a *app) askCRDUpdates(p *out.Printer, plan *crdPlan) (bool, error) {
	for _, n := range plan.Newer {
		p.Line("  %s left alone, newer on the cluster: %s (another product, such as a service mesh, may own it)", p.Mark("warn"), n)
	}
	if len(plan.Update) == 0 {
		return false, nil
	}
	p.Line("These %d dependency CRDs exist and differ from the ones this ambit needs:", len(plan.Update))
	for _, n := range plan.Update {
		p.Line("  %s", n)
	}
	p.Line("A second agentgateway or ToolHive installation on the same cluster is not supported by Ambit.")
	if err := kube.Confirm(p, a.in, a.stdinTTY(), a.g.yes, nil, fmt.Sprintf("Update these %d CRDs?", len(plan.Update))); err != nil {
		return false, err
	}
	return true, nil
}

// pendingRefusal is the error for a release left in pending-install or pending-upgrade (spec D2 6.3).
func pendingRefusal(pe *helmx.PendingError, ns string) *out.Error {
	return &out.Error{What: fmt.Sprintf("the release %s is %s since %s (revision %d)", pe.Name, pe.Status, pe.Started.UTC().Format(time.RFC3339), pe.Revision),
		Why:  "another install or upgrade may be running",
		Next: fmt.Sprintf("check that no helm or ambit command is working on it (helm history %s -n %s), then: ambit upgrade --recover-pending", pe.Name, ns)}
}

// findReleases finds the releases of the charts ambit and ambit-crds in the namespace by chart name, so an install
// made by hand with other release names is found (spec D2 6.1). nil: not installed.
func findReleases(h helmx.Client, ns string) (app, crds *helmx.Release, err error) {
	rs, err := h.List(false)
	if err != nil {
		return nil, nil, &out.Error{What: "the Helm releases in " + ns + " cannot be read", Why: err.Error(), Next: "ask for get and list on Secrets in " + ns}
	}
	found := map[string][]helmx.Release{}
	for _, r := range rs {
		if r.Chart == "ambit" || r.Chart == "ambit-crds" {
			found[r.Chart] = append(found[r.Chart], r)
		}
	}
	for _, chart := range []string{"ambit", "ambit-crds"} {
		if len(found[chart]) > 1 {
			var names []string
			for _, r := range found[chart] {
				names = append(names, r.Name)
			}
			return nil, nil, &out.Error{What: "more than one release of chart " + chart + " in " + ns, Why: "releases: " + strings.Join(names, ", "),
				Next: "remove the extra release with helm uninstall, then run the command again"}
		}
	}
	if l := found["ambit"]; len(l) == 1 {
		app = &l[0]
	}
	if l := found["ambit-crds"]; len(l) == 1 {
		crds = &l[0]
	}
	return app, crds, nil
}

func (a *app) uninstall(ctx context.Context) error {
	p := a.printer()
	c, h, err := a.target("")
	if err != nil {
		return err
	}
	ns := a.g.namespace
	app, crds, err := findReleases(h, ns)
	if err != nil {
		return err
	}
	if app == nil && crds == nil {
		return &out.Error{What: "Ambit is not installed in namespace " + ns, Why: "there is no Helm release of chart ambit or ambit-crds there",
			Next: "pass -n <namespace> if it is installed in another namespace (helm list -A)"}
	}
	p.Line("Uninstalling Ambit from context %s (%s), namespace %s:", c.Context, c.Server, ns)
	for _, r := range []*helmx.Release{app, crds} {
		if r != nil {
			p.Line("  release %s (chart %s %s, revision %d)", r.Name, r.Chart, r.ChartVersion, r.Revision)
		}
	}
	p.Line("This stops every gateway. CRDs, your objects and the audit volumes stay.")
	if err := a.confirmByName(p, c.Context); err != nil {
		return err
	}
	step := stepper(p)
	var done []string
	for _, r := range []*helmx.Release{app, crds} {
		if r == nil {
			continue
		}
		if err := step("Uninstalled "+r.Name, func() error { _, err := h.Uninstall(r.Name); return err }); err != nil {
			return &out.Error{What: "the release " + r.Name + " was not uninstalled", Why: err.Error(), Next: "helm uninstall " + r.Name + " -n " + ns}
		}
		done = append(done, fmt.Sprintf("%s (revision %d)", r.Name, r.Revision))
	}
	p.Line("%s Uninstalled %s from context %s, namespace %s", p.Mark("ok"), strings.Join(done, " and "), c.Context, ns)
	p.Line("Left in the cluster (by design):")
	p.Line("  CRDs and your objects: kubectl get crd -o name | grep ambitproject.dev")
	p.Line("  ConfigMap ambit-access-mode: kubectl delete configmap -n %s ambit-access-mode", ns)
	p.Line("  ClickHouse volumes (audit records): kubectl get pvc -n %s", ns)
	p.Line("  Secrets that ambit install --secret-file wrote: kubectl get secret -n %s -l %s=%s", ns, createdByLabel, createdByValue)
	p.Line("  Dependency CRDs (Gateway API, agentgateway, ToolHive): other products may use them; delete only if none does")
	return nil
}

// confirmByName asks the person to type the context name (decision CLI-9): a "y" is too easy for a command that
// stops every gateway. --yes skips it; without a terminal --yes is required.
func (a *app) confirmByName(p *out.Printer, kctx string) error {
	if a.g.yes {
		return nil
	}
	if !a.stdinTTY() {
		return out.Usage("no confirmation", "there is no terminal to ask in", "run the command again with --yes")
	}
	p.Line("Type the context name (%s) to uninstall:", kctx)
	answer, _ := bufio.NewReader(a.in).ReadString('\n')
	if strings.TrimSpace(answer) != kctx {
		return &out.Error{What: "cancelled", Why: fmt.Sprintf("the answer %q is not the context name %q", strings.TrimSpace(answer), kctx), Code: out.ExitFailed}
	}
	return nil
}
