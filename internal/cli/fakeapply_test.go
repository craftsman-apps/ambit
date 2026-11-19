package cli

import (
	"context"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/craftsman-apps/ambit/internal/cli/kube"
)

// fakeCtrl is controller-runtime's fake client, except that a server-side apply with dryRun=All stores nothing
// and gives the object back unchanged, as the API server does. (The fake moves the dry-run option of Apply into
// PatchOptions.Raw and then stores the object, controller-runtime v0.25.2.) applied counts the real applies. Its
// REST mapper knows the kinds of kube.Scheme, as a cluster with Ambit's CRDs does; other kinds are unknown.
func fakeCtrl(applied *int, objs ...client.Object) client.Client {
	return crfake.NewClientBuilder().WithScheme(kube.Scheme).WithRESTMapper(testrestmapper.TestOnlyStaticRESTMapper(kube.Scheme)).
		WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			o := &client.ApplyOptions{}
			o.ApplyOptions(opts)
			if slices.Contains(o.DryRun, metav1.DryRunAll) {
				return nil
			}
			if applied != nil {
				*applied++
			}
			return c.Apply(ctx, obj, opts...)
		},
	}).Build()
}
