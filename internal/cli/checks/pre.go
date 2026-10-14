package checks

import (
	"context"
	"fmt"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftsman-apps/ambit/internal/cli/kube"
)

// MinKubernetes is the oldest Kubernetes the chart supports (docs/upgrading.md).
const MinKubernetes = "1.30"

const gatewayAPIVersionAnnotation = "gateway.networking.k8s.io/bundle-version"

// R1: Kubernetes server version at least 1.30.
func r1KubeVersion(_ context.Context, e *Env) Result {
	v, err := e.Kube.Discovery().ServerVersion()
	if err != nil {
		return unknownFor(err, "read", "the server version", "", "live objects")
	}
	have, err := version.ParseGeneric(v.Major + "." + strings.TrimSuffix(v.Minor, "+") + ".0")
	if err != nil {
		return res(Unknown, "live objects", "server version %q cannot be read: %v", v.GitVersion, err)
	}
	if have.LessThan(version.MustParseGeneric(MinKubernetes + ".0")) {
		return res(Fail, "live objects", "Kubernetes %s.%s is older than %s", v.Major, v.Minor, MinKubernetes).
			next("upgrade the cluster to Kubernetes " + MinKubernetes + " or later")
	}
	return res(Pass, "live objects", "Kubernetes %s.%s (%s or later is required)", v.Major, strings.TrimSuffix(v.Minor, "+"), MinKubernetes)
}

// R2: the user can create CRDs, namespaces and ClusterRoles (SelfSubjectAccessReview).
func r2Permissions(ctx context.Context, e *Env) Result {
	var missing []string
	for _, a := range []authv1.ResourceAttributes{
		{Verb: "create", Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
		{Verb: "create", Resource: "namespaces"},
		{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "clusterroles"},
	} {
		a := a
		r, err := e.Kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx,
			&authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &a}}, metav1.CreateOptions{})
		if err != nil {
			return unknownFor(err, "create", "selfsubjectaccessreviews", "", "live objects")
		}
		if !r.Status.Allowed {
			missing = append(missing, a.Verb+" "+a.Resource)
		}
	}
	if len(missing) > 0 {
		return res(Fail, "live objects", "you cannot %s", strings.Join(missing, ", ")).
			next("installing Ambit needs cluster-admin or an equal role")
	}
	return res(Pass, "live objects", "you can create CRDs, namespaces and ClusterRoles")
}

// R3: which releases are installed.
func r3Installed(_ context.Context, e *Env) Result {
	crds, err := e.Helm.Get("ambit-crds")
	if err != nil {
		return unknownFor(err, "get", "secrets", e.Namespace, "live objects")
	}
	app, err := e.Helm.Get("ambit")
	if err != nil {
		return unknownFor(err, "get", "secrets", e.Namespace, "live objects")
	}
	switch {
	case crds == nil && app == nil:
		return res(Pass, "live objects", "neither ambit-crds nor ambit is installed in %s (fresh install)", e.Namespace)
	case crds != nil && app != nil && crds.ChartVersion == app.ChartVersion:
		return res(Pass, "live objects", "ambit-crds and ambit %s are installed in %s (an install becomes an upgrade)", app.ChartVersion, e.Namespace)
	case crds != nil && app != nil:
		return res(Warn, "live objects", "ambit-crds %s and ambit %s are installed at different versions", crds.ChartVersion, app.ChartVersion).
			next("upgrade both to the same version")
	case crds != nil:
		return res(Warn, "live objects", "only ambit-crds %s is installed; ambit is missing", crds.ChartVersion)
	default:
		return res(Warn, "live objects", "only ambit %s is installed; ambit-crds is missing", app.ChartVersion)
	}
}

// R4: a default StorageClass exists when the chart's own ClickHouse is on (it keeps its data on a volume). The
// chart's Valkey keeps no data on disk, so it needs none (ruling S11).
func r4StorageClass(ctx context.Context, e *Env) Result {
	v := e.ValuesFile
	if v == nil {
		return res(NA, "values", "no values file given (-f values.yaml)")
	}
	if !boolean(v, "evidence.enabled") || str(v, "evidence.clickhouse.external.host") != "" {
		return res(NA, "values", "the chart's own ClickHouse is not used")
	}
	var list storagev1.StorageClassList
	if err := e.Ctrl.List(ctx, &list); err != nil {
		return unknownFor(err, "list", "storageclasses", "", "values and live objects")
	}
	for _, sc := range list.Items {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			return res(Pass, "values and live objects", "default StorageClass %s holds the ClickHouse volume", sc.Name)
		}
	}
	return res(Fail, "values and live objects", "the chart's ClickHouse needs a volume, but there is no default StorageClass").
		next("mark a StorageClass as default, or set evidence.clickhouse.external.host")
}

// R5: the TLS Secret named in values exists.
func r5TLSSecret(ctx context.Context, e *Env) Result {
	v := e.ValuesFile
	if v == nil {
		return res(NA, "values", "no values file given (-f values.yaml)")
	}
	if !boolean(v, "gateway.tls.enabled") {
		return res(NA, "values", "gateway.tls.enabled is false")
	}
	name := str(v, "gateway.tls.certificateSecret")
	if name == "" {
		return res(Fail, "values", "gateway.tls.enabled is true but gateway.tls.certificateSecret is empty").
			next("set gateway.tls.certificateSecret to a kubernetes.io/tls Secret")
	}
	var s corev1.Secret
	err := e.Ctrl.Get(ctx, client.ObjectKey{Namespace: e.Namespace, Name: name}, &s)
	switch {
	case apierrors.IsNotFound(err):
		return res(Fail, "values and live objects", "TLS Secret %s does not exist in %s", name, e.Namespace).
			next(fmt.Sprintf("kubectl -n %s create secret tls %s --cert=<file> --key=<file>", e.Namespace, name))
	case err != nil:
		return unknownFor(err, "get", "secrets", e.Namespace, "values and live objects")
	}
	return res(Pass, "values and live objects", "TLS Secret %s exists", name)
}

// R6: third-party CRDs installed by another tool are not older than the ones this version needs (spec 4.8).
func r6ThirdPartyCRDs(ctx context.Context, e *Env) Result {
	want, err := kube.Decode(e.CRDBundle)
	if err != nil {
		return res(Unknown, "live objects", "the embedded CRD bundle cannot be read: %v", err)
	}
	var older, present []string
	for _, w := range want {
		if w.GetKind() != "CustomResourceDefinition" {
			continue
		}
		var have apiextensionsv1.CustomResourceDefinition
		err := e.Ctrl.Get(ctx, client.ObjectKey{Name: w.GetName()}, &have)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return unknownFor(err, "get", "customresourcedefinitions", "", "live objects")
		}
		present = append(present, w.GetName())
		if req := w.GetAnnotations()[gatewayAPIVersionAnnotation]; req != "" {
			if got := have.Annotations[gatewayAPIVersionAnnotation]; got != "" && semverLess(got, req) {
				older = append(older, fmt.Sprintf("%s is Gateway API %s, %s is required", w.GetName(), got, req))
				continue
			}
		}
		served := map[string]bool{}
		for _, v := range have.Spec.Versions {
			served[v.Name] = v.Served
		}
		vs, _, _ := unstructuredVersions(w.Object)
		for _, v := range vs {
			if !served[v] {
				older = append(older, fmt.Sprintf("%s does not serve %s", w.GetName(), v))
			}
		}
	}
	switch {
	case len(older) > 0:
		return res(Fail, "live objects", "third-party CRDs are older than this version needs: %s", strings.Join(older, "; ")).
			next("apply the newer CRDs: kubectl apply --server-side --force-conflicts -f crds.yaml (from the release)")
	case len(present) == 0:
		return res(Pass, "live objects", "no third-party CRDs yet; they are applied on install")
	}
	return res(Pass, "live objects", "%d third-party CRDs are present and new enough", len(present))
}

func semverLess(a, b string) bool {
	va, err1 := version.ParseSemantic(a)
	vb, err2 := version.ParseSemantic(b)
	return err1 == nil && err2 == nil && va.LessThan(vb)
}

func unstructuredVersions(o map[string]any) ([]string, bool, error) {
	spec, _ := o["spec"].(map[string]any)
	list, _ := spec["versions"].([]any)
	var out []string
	for _, v := range list {
		m, _ := v.(map[string]any)
		if served, _ := m["served"].(bool); served {
			out = append(out, fmt.Sprint(m["name"]))
		}
	}
	return out, true, nil
}
