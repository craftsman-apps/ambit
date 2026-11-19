package cli

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftsman-apps/ambit/internal/cli/helmx"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
	"github.com/craftsman-apps/ambit/internal/cli/local"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// dryRunManager is the field manager of the server-side dry runs; nothing is stored under it.
const dryRunManager = "ambit-cli-dry-run"

// dryRun is what `install --dry-run` and `upgrade --dry-run` do after the preflight (spec D2 3.2): the dependency
// CRDs by server-side apply with dryRun=All, both charts rendered, and each rendered object whose kind exists on
// the cluster validated by the API server (a server-side apply with dryRun=All). Objects whose CRD is not installed
// yet are listed as not validated. Nothing is written: no Secret, no CRD, no release.
func (a *app) dryRun(ctx context.Context, p *out.Printer, c *kube.Clients, h helmx.Client, charts local.Charts, vals map[string]any,
	plan *crdPlan, secrets []secretFile) error {
	ns := a.g.namespace
	if err := plan.apply(ctx, c.Ctrl, true, true); err != nil {
		return &out.Error{What: "the API server refused the dependency CRDs (dry run)", Why: err.Error(), Next: "add --verbose for details"}
	}
	line := fmt.Sprintf("Would apply %d dependency CRDs", len(plan.Create)+len(plan.Update))
	if len(plan.Newer) > 0 {
		line += fmt.Sprintf(" (%d skipped: newer on the cluster: %s)", len(plan.Newer), strings.Join(plan.Newer, ", "))
	}
	p.Line("%s", line)
	if len(plan.Update) > 0 {
		p.Line("  %d of them exist and would change; without --dry-run the CLI asks first: %s", len(plan.Update), strings.Join(plan.Update, ", "))
	}

	nsExists := true
	if _, err := c.Kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		nsExists = false
	}
	kv := ""
	if v, err := c.Kube.Discovery().ServerVersion(); err == nil {
		kv = v.GitVersion
	}
	type result struct{ object, status string }
	var counts []string
	var results []result
	validated, notValidated := 0, 0
	var invalid []string
	for _, rel := range []struct {
		name string
		vals map[string]any
	}{{"ambit-crds", nil}, {"ambit", vals}} {
		m, err := h.Render(ctx, rel.name, charts.Ref(rel.name), charts.Version, rel.vals, kv)
		if err != nil {
			return &out.Error{What: "the chart " + rel.name + " does not render with these values", Why: err.Error(), Next: "fix the values file"}
		}
		objs, err := kube.Decode([]byte(m))
		if err != nil {
			return &out.Error{What: "the rendered chart " + rel.name + " cannot be read", Why: err.Error(), Next: "report this at https://github.com/craftsman-apps/ambit/issues"}
		}
		counts = append(counts, fmt.Sprintf("%s %s (%d objects)", rel.name, chartVersion(charts), len(objs)))
		for _, u := range objs {
			status, err := validateObject(ctx, c.Ctrl, u, ns, nsExists)
			name := u.GetKind() + " " + u.GetName()
			switch {
			case err != nil:
				invalid = append(invalid, name+": "+err.Error())
				status = "refused by the API server: " + err.Error()
			case status == "validated":
				validated++
			default:
				notValidated++
			}
			results = append(results, result{name, status})
		}
	}
	p.Line("Would install %s in %s", strings.Join(counts, " and "), ns)
	p.Line("  validated by the API server: %d; not validated (CRD not installed yet): %d", validated, notValidated)
	if !nsExists {
		p.Line("  the namespace %s does not exist yet: its objects were validated in the namespace default", ns)
	}
	for _, r := range results {
		p.Line("    %s: %s", r.object, r.status)
	}
	for _, s := range describeSecretFiles(secrets) {
		p.Line("Would create or update %s", s)
	}
	if len(invalid) > 0 {
		return &out.Error{What: fmt.Sprintf("the API server refused %d objects (dry run)", len(invalid)), Why: strings.Join(invalid, "; "),
			Next: "fix the values file, then run the command again"}
	}
	p.Line("%s dry run: nothing was changed", p.Mark("ok"))
	return nil
}

// validateObject asks the API server to validate u by a server-side apply with dryRun=All. A kind the cluster does
// not know yet (its CRD comes with the same install) is not validated. When the namespace does not exist yet, a
// namespaced object is validated in the namespace default.
func validateObject(ctx context.Context, c client.Client, u *unstructured.Unstructured, ns string, nsExists bool) (string, error) {
	gvk := u.GroupVersionKind()
	m, err := c.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		return "not validated (CRD not installed yet)", nil
	}
	if err != nil {
		return "", err
	}
	o := u.DeepCopy()
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		if o.GetNamespace() == "" || o.GetNamespace() == ns {
			o.SetNamespace(ns)
			if !nsExists {
				o.SetNamespace(metav1.NamespaceDefault)
			}
		}
	} else {
		o.SetNamespace("")
	}
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(o), client.FieldOwner(dryRunManager), client.ForceOwnership, client.DryRunAll); err != nil {
		return "", err
	}
	return "validated", nil
}
