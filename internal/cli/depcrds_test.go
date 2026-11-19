package cli

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/craftsman-apps/ambit/internal/cli/assets"
	"github.com/craftsman-apps/ambit/internal/cli/kube"
)

// agentgateway and ToolHive CRDs carry no version label: a CRD that equals the bundle is left alone, one that
// differs is listed for the question.
func TestPlanCRDsComparesUnversionedCRDs(t *testing.T) {
	ctx := context.Background()
	c := crfake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	bundle := assets.CRDBundle()
	p, err := planCRDs(ctx, c, bundle)
	if err != nil || len(p.Create) != 28 || len(p.Update) != 0 {
		t.Fatalf("empty cluster: %+v, %v", p, err)
	}
	if err := p.apply(ctx, c, false, false); err != nil {
		t.Fatal(err)
	}
	p, err = planCRDs(ctx, c, bundle)
	if err != nil || len(p.Create) != 0 || len(p.Update) != 0 || len(p.Newer) != 0 {
		t.Fatalf("after apply: create %v update %v newer %v, %v", p.Create, p.Update, p.Newer, err)
	}
	if p.summary(false) != "all up to date" {
		t.Fatalf("summary = %q", p.summary(false))
	}
	objs, _ := kube.Decode(bundle)
	for _, o := range objs {
		if o.GetAnnotations()[gatewayBundleVersion] == "" && o.GetKind() == "CustomResourceDefinition" {
			changed := o.DeepCopy()
			changed.Object["spec"].(map[string]any)["names"].(map[string]any)["shortNames"] = []any{"changed-by-another-install"}
			if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(changed), client.FieldOwner("other"), client.ForceOwnership); err != nil {
				t.Fatal(err)
			}
			p, err = planCRDs(ctx, c, bundle)
			if err != nil || len(p.Update) != 1 || p.Update[0] != o.GetName() {
				t.Fatalf("changed %s: update %v, %v", o.GetName(), p.Update, err)
			}
			return
		}
	}
	t.Fatal("the bundle has no CRD without a version label")
}
