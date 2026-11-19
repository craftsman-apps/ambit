package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/craftsman-apps/ambit/internal/cli/assets"
	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
	"github.com/craftsman-apps/ambit/internal/cli/local"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// pendingMinAge is how old a pending revision must be before --recover-pending marks it failed: longer than the
// default --timeout of 10 minutes, so a normal run by someone else has finished or failed by then (spec D2 6.3).
const pendingMinAge = 15 * time.Minute

// recoverNote is the description --recover-pending writes on the revision it marks failed.
const recoverNote = "marked failed by ambit upgrade --recover-pending"

// upgradeNotes are the sections of docs/upgrading.md and the release that introduced each. `ambit upgrade` names
// the ones between the installed version and its own. TestUpgradeNotesMatchTheDocs keeps them in step.
var upgradeNotes = []struct{ version, heading string }{
	{"0.1.0", "To the release with personal API keys"},
	{"0.2.0", "To the release with audit-only mode"},
	{"0.2.3", "To the release with access rules that name people"},
	{"0.4.0", "To the release with agentgateway v1.7.0"},
	{"0.5.0", "To the release with service accounts (0.5.0)"},
}

type upgradeFlags struct {
	installFlags
	reuseValues, recoverPending bool
}

func (a *app) upgradeCmd() *cobra.Command {
	var f upgradeFlags
	cmd := &cobra.Command{
		Use:   "upgrade (-f values.yaml | --reuse-values)",
		Short: "Upgrade Ambit to the version of this CLI: the dependency CRDs, then the Helm releases ambit-crds and ambit",
		Long: `Upgrades the Helm releases of the charts ambit-crds and ambit (found by chart name) to this CLI's version.
One of -f or --reuse-values is required. It shows the target and asks first. A team can mix ambit upgrade and
helm upgrade freely. Exit code 3: Ambit is upgraded and healthy, but the production checklist has failures.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error { return a.upgrade(cmd.Context(), f) }),
	}
	fl := cmd.Flags()
	fl.StringArrayVarP(&f.values, "values", "f", nil, "values file; may repeat: later files win, as in Helm")
	fl.BoolVar(&f.reuseValues, "reuse-values", false, "keep the values stored in the release, on top of the new chart's defaults (Helm's --reset-then-reuse-values)")
	fl.StringArrayVar(&f.secretFiles, "secret-file", nil, "create or update a Secret that the values name, from a file: <secret>/<key>=<path>; may repeat")
	fl.BoolVar(&f.dryRun, "dry-run", false, "check and render everything and let the API server validate it; change nothing")
	fl.DurationVar(&f.timeout, "timeout", 10*time.Minute, "how long to wait for the workloads")
	fl.BoolVar(&f.recoverPending, "recover-pending", false, "mark a release left in pending-install or pending-upgrade (15 minutes or older) as failed, then upgrade; asks first")
	fl.StringVar(&f.chartRepo, "chart-repo", ChartRepo, "OCI repository of the charts, for a mirror")
	fl.StringVar(&f.registryConfig, "registry-config", "", "Helm registry config for a private registry (default: Helm's)")
	addChartDirFlag(cmd, &f.chartDir)
	return cmd
}

func (a *app) upgrade(ctx context.Context, f upgradeFlags) error {
	p := a.printer()
	charts, err := chartsFor("ambit upgrade -f values.yaml", f.chartDir, f.chartRepo)
	if err != nil {
		return err
	}
	switch {
	case len(f.values) > 0 && f.reuseValues:
		return out.Usage("-f and --reuse-values", "cannot be used together", "pass -f values.yaml, or --reuse-values")
	case len(f.values) == 0 && !f.reuseValues:
		return out.Usage("no values", "ambit upgrade needs -f values.yaml, or --reuse-values to keep the values stored in the release",
			"ambit upgrade -f values.yaml")
	}
	var vals map[string]any
	if len(f.values) > 0 {
		if vals, err = local.MergeValues(map[string]any{}, f.values); err != nil {
			return out.Usage("-f", err.Error(), "check the values files")
		}
	}
	c, h, err := a.target(f.registryConfig)
	if err != nil {
		return err
	}
	ns := a.g.namespace

	// Release discovery (spec D2 6.1).
	app, crds, err := findReleases(h, ns)
	if err != nil {
		return err
	}
	if app == nil {
		if err := refuseOtherTool(ctx, c, ns, nil); err != nil {
			return err
		}
		return &out.Error{What: "Ambit is not installed in namespace " + ns, Why: "there is no Helm release of chart ambit there",
			Next: "ambit install -f values.yaml (or pass -n <namespace>; helm list -A shows the releases)"}
	}
	names := relNames{crds: "ambit-crds", app: app.Name}
	if crds != nil {
		names.crds = crds.Name
	}
	for _, r := range []*helmx.Release{app, crds} {
		if r != nil && r.Name != r.Chart {
			p.Line("Found the release %s of chart %s", r.Name, r.Chart)
		}
	}
	if f.reuseValues {
		if vals, err = h.Values(app.Name, false); err != nil {
			return &out.Error{What: "the values of the release " + app.Name + " cannot be read", Why: err.Error(), Next: "ask for get on Secrets in " + ns + ", or pass -f values.yaml"}
		}
	}
	secrets, err := readSecretFiles(f.secretFiles, vals, p.Redactor())
	if err != nil {
		return err
	}

	// A downgrade is refused: an old CLI would install a chart whose checks it does not know (decision CLI-4).
	if charts.Dir == "" && semverLess(charts.Version, app.ChartVersion) {
		return &out.Error{What: "the installed Ambit is newer than this CLI",
			Why:  fmt.Sprintf("ambit %s is installed; this CLI installs %s", app.ChartVersion, charts.Version),
			Next: fmt.Sprintf("use an ambit CLI of version %s, or helm rollback %s <revision> -n %s", app.ChartVersion, app.Name, ns)}
	}

	// A pending release is refused, or with --recover-pending checked for its age (spec D2 6.3).
	var pending []*helmx.Release
	for _, r := range []*helmx.Release{crds, app} {
		if r == nil || !strings.HasPrefix(r.Status, "pending-") {
			continue
		}
		if !f.recoverPending {
			return pendingRefusal(&helmx.PendingError{Name: r.Name, Status: r.Status, Revision: r.Revision, Started: r.Updated, Description: r.Description}, ns)
		}
		if age := time.Since(r.Updated); age < pendingMinAge {
			return &out.Error{What: fmt.Sprintf("revision %d of %s started %s ago", r.Revision, r.Name, age.Round(time.Minute)),
				Why:  "a run by someone else may still be working on it; --recover-pending waits until the revision is 15 minutes old",
				Next: fmt.Sprintf("wait until %s UTC, or check with the person who started it", r.Updated.Add(pendingMinAge).UTC().Format("15:04"))}
		}
		pending = append(pending, r)
	}

	// 1. Target and question.
	target := fmt.Sprintf("Upgrade Ambit %s → %s in context %s (%s), namespace %s", app.ChartVersion, chartVersion(charts), c.Context, c.Server, ns)
	if app.ChartVersion == charts.Version {
		p.Line("Ambit %s is installed: the upgrade applies the same version again (a repair)", app.ChartVersion)
	}
	for _, l := range upgradeLinks(app.ChartVersion, charts.Version) {
		p.Line("%s", l)
	}
	if f.dryRun {
		p.Line("%s (dry run)", target)
	} else if err := kube.Confirm(p, a.stdin(), a.stdinTTY(), a.g.yes, nil, target+"?"); err != nil {
		return err
	}
	step := stepper(p)

	// 2. Preflight.
	var plan *crdPlan
	if err := step("Preflight", func() error {
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
		return a.dryRun(ctx, p, c, h, charts, vals, plan, secrets, "upgrade")
	}
	for _, r := range pending {
		p.Line("The release %s is %s: revision %d, started %s (%s ago)", r.Name, r.Status, r.Revision, r.Updated.UTC().Format(time.RFC3339), time.Since(r.Updated).Round(time.Minute))
		if r.Description != "" {
			p.Line("  Helm's note on it: %s", r.Description)
		}
		if err := kube.Confirm(p, a.stdin(), a.stdinTTY(), a.g.yes, nil, fmt.Sprintf("Mark revision %d of %s as failed and upgrade?", r.Revision, r.Name)); err != nil {
			return err
		}
	}
	update, err := a.askCRDUpdates(p, plan)
	if err != nil {
		return err
	}
	if len(secrets) > 0 {
		if err := step("Secrets: "+strings.Join(describeSecretFiles(secrets), "; "), func() error {
			return writeSecretFiles(ctx, c.Kube, ns, secrets, vals)
		}); err != nil {
			return &out.Error{What: "the Secrets were not written", Why: err.Error(), Next: "check that you can create Secrets in " + ns + ", then run ambit upgrade again"}
		}
	}
	opts := helmx.ApplyOptions{ResetThenReuseValues: f.reuseValues, RecoverPending: len(pending) > 0, RecoverNote: recoverNote}
	if f.reuseValues {
		vals = nil
	}
	return a.applyAndCheck(ctx, p, c, h, charts, vals, plan, update, f.timeout, "upgraded", "ambit upgrade", names, opts)
}

// upgradeLinks names the sections of docs/upgrading.md for the releases after from, up to to.
func upgradeLinks(from, to string) []string {
	var lines []string
	for _, n := range upgradeNotes {
		if semverLess(from, n.version) && !semverLess(to, n.version) {
			title := n.heading
			if !strings.Contains(title, "("+n.version+")") {
				title += " (" + n.version + ")"
			}
			lines = append(lines, fmt.Sprintf("  %s: https://ambitproject.dev/docs/upgrading#%s", title, anchor(n.heading)))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{"Read these notes of docs/upgrading.md before you upgrade:"}, lines...)
}

// anchor is the heading's link anchor as the docs site makes it: lower case, spaces to '-', other marks dropped.
func anchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}
