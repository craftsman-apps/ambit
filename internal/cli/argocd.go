package cli

import (
	"path"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/craftsman-apps/ambit/internal/cli/assets"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// chartRegistry is ChartRepo as Argo CD names an OCI Helm repository: without the oci:// scheme.
const chartRegistry = "ghcr.io/craftsman-apps/charts"

// argoFiles are the files of spec D2 3.1.1 for a configuration repository, by path from the repository root:
// the CRD bundle, and one Argo CD Application each for the dependency CRDs (wave -2), the chart ambit-crds
// (wave -1), the chart ambit with the values from the repository (wave 0) and the Ambit objects. With g.parent
// a parent Application holds the four; sync waves order them only then.
func argoFiles(g *gitopsAnswers, ns string) map[string][]byte {
	repo := "https://github.com/" + g.repository + ".git"
	files := map[string][]byte{path.Join(g.path, "crds", "dependency-crds.yaml"): assets.CRDBundle()}
	app := func(name, wave string, spec map[string]any) []byte {
		spec["project"] = "default"
		if _, ok := spec["destination"]; !ok {
			spec["destination"] = map[string]any{"server": "https://kubernetes.default.svc", "namespace": ns}
		}
		b, _ := yaml.Marshal(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
			"metadata": map[string]any{"name": name, "namespace": "argocd", "annotations": map[string]string{"argocd.argoproj.io/sync-wave": wave}},
			"spec":     spec})
		return b
	}
	// retry: a chart synced before its CRDs are Established fails once; Argo CD tries again instead of waiting for
	// the next commit.
	retry := map[string]any{"limit": 10, "backoff": map[string]any{"duration": "10s", "factor": 2, "maxDuration": "3m"}}
	automated := map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}, "retry": retry}
	withOptions := func(opts ...string) map[string]any {
		m := map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}, "retry": retry}
		var o []any
		for _, s := range opts {
			o = append(o, s)
		}
		m["syncOptions"] = o
		return m
	}
	files["argocd/ambit-dependency-crds.yaml"] = app("ambit-dependency-crds", "-2", map[string]any{
		"source":     map[string]any{"repoURL": repo, "targetRevision": g.branch, "path": path.Join(g.path, "crds")},
		"syncPolicy": withOptions("ServerSideApply=true"),
	})
	files["argocd/ambit-crds.yaml"] = app("ambit-crds", "-1", map[string]any{
		"source":     map[string]any{"repoURL": chartRegistry, "chart": "ambit-crds", "targetRevision": version},
		"syncPolicy": withOptions("CreateNamespace=true"),
	})
	files["argocd/ambit.yaml"] = app("ambit", "0", map[string]any{
		"sources": []any{
			map[string]any{"repoURL": chartRegistry, "chart": "ambit", "targetRevision": version,
				"helm": map[string]any{"releaseName": "ambit", "valueFiles": []any{"$values/" + path.Join(g.path, "values.yaml")}}},
			map[string]any{"repoURL": repo, "targetRevision": g.branch, "ref": "values"},
		},
		"syncPolicy": withOptions("CreateNamespace=true"),
	})
	files["argocd/ambit-objects.yaml"] = app("ambit-objects", "1", map[string]any{
		"source": map[string]any{"repoURL": repo, "targetRevision": g.branch, "path": g.path,
			// Only the object directories: values.yaml and crds/ beside them are not for this Application.
			"directory": map[string]any{"recurse": true, "include": "{accessrules/*.yaml,servers/*.yaml,models/*.yaml}"}},
		"syncPolicy": automated,
	})
	if g.parent {
		files["argocd-parent.yaml"] = app("ambit-parent", "0", map[string]any{
			"source":      map[string]any{"repoURL": repo, "targetRevision": g.branch, "path": "argocd"},
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "argocd"},
			"syncPolicy":  automated,
		})
	}
	return files
}

// printArgoNext says how to start the Applications and which health check Argo CD needs.
func printArgoNext(p *out.Printer, g *gitopsAnswers) {
	p.Line("Wrote the Argo CD files: %s/crds/dependency-crds.yaml and argocd/ (ambit-dependency-crds, ambit-crds, ambit, ambit-objects)", g.path)
	p.Line("Commit them to %s (branch %s), then:", g.repository, g.branch)
	if g.parent {
		p.Line("  kubectl apply -n argocd -f argocd-parent.yaml   # the parent Application syncs the four in order (sync waves)")
	} else {
		p.Line("  Apply them in this order, and wait until each is Healthy (sync waves order them only under a parent Application):")
		for _, f := range []string{"ambit-dependency-crds", "ambit-crds", "ambit", "ambit-objects"} {
			p.Line("  kubectl apply -n argocd -f argocd/%s.yaml", f)
		}
	}
	p.Line("Argo CD needs Ambit's health check in argocd-cm (deploy/argocd/health.lua of the repository), one key per kind:")
	for _, k := range []string{"Model", "Server", "AccessRule"} {
		p.Line("  resource.customizations.health.ambitproject.dev_%s: <the content of health.lua>", k)
	}
	if g.parent {
		p.Line("For the parent to wait until each child is Healthy before the next wave, Argo CD also needs a health check for")
		p.Line("  Applications (resource.customizations.health.argoproj.io_Application; see Argo CD's \"App of Apps\" docs).")
	}
	p.Line("To upgrade: run ambit init --gitops argocd --force with the newer ambit, and review the diff in a pull request.")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
