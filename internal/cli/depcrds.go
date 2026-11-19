package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftsman-apps/ambit/internal/cli/kube"
)

// cliFieldManager is the server-side apply field manager of the dependency CRDs that install and upgrade apply
// (spec D2 3.2 step 4). local up keeps kube.FieldManager.
const cliFieldManager = "ambit-cli"

const gatewayBundleVersion = "gateway.networking.k8s.io/bundle-version"

// crdPlan is what install and upgrade do with the dependency CRD bundle (Gateway API, agentgateway, ToolHive).
type crdPlan struct {
	Create []string // CRDs that do not exist: created without a question
	Update []string // older Gateway API CRDs, or agentgateway and ToolHive CRDs that differ: asked first
	Newer  []string // Gateway API CRDs with a newer bundle version on the cluster: left alone, with a warning
	// gatewayCreate, gatewayUpdate: a Gateway API CRD is created or updated, so the bundle's other objects (the
	// Gateway API admission policy and its binding) are applied too. Otherwise they are applied only when they
	// do not exist.
	gatewayCreate, gatewayUpdate bool
	objs                         []*unstructured.Unstructured
	missing                      map[string]bool // non-CRD objects that do not exist
}

// planCRDs compares the bundle with the cluster. It reads CRDs and, for CRDs without a version label, makes a
// server-side dry-run apply to see whether the bundle would change them. It writes nothing.
func planCRDs(ctx context.Context, c client.Client, bundle []byte) (*crdPlan, error) {
	objs, err := kube.Decode(bundle)
	if err != nil {
		return nil, fmt.Errorf("the embedded CRD bundle cannot be read: %w", err)
	}
	p := &crdPlan{objs: objs, missing: map[string]bool{}}
	for _, w := range objs {
		if w.GetKind() != "CustomResourceDefinition" {
			have := &unstructured.Unstructured{}
			have.SetGroupVersionKind(w.GroupVersionKind())
			err := c.Get(ctx, client.ObjectKey{Name: w.GetName(), Namespace: w.GetNamespace()}, have)
			if apierrors.IsNotFound(err) {
				p.missing[w.GetKind()+"/"+w.GetName()] = true
			} else if err != nil {
				return nil, fmt.Errorf("read %s %s: %w", w.GetKind(), w.GetName(), err)
			}
			continue
		}
		name := w.GetName()
		var have apiextensionsv1.CustomResourceDefinition
		err := c.Get(ctx, client.ObjectKey{Name: name}, &have)
		if apierrors.IsNotFound(err) {
			p.Create = append(p.Create, name)
			p.gatewayCreate = p.gatewayCreate || w.GetAnnotations()[gatewayBundleVersion] != ""
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read CRD %s: %w", name, err)
		}
		if want := w.GetAnnotations()[gatewayBundleVersion]; want != "" {
			got := have.Annotations[gatewayBundleVersion]
			switch {
			case got == want:
			case got != "" && semverLess(want, got):
				p.Newer = append(p.Newer, fmt.Sprintf("%s (%s on the cluster, %s in ambit)", name, got, want))
			default:
				p.Update = append(p.Update, name)
				p.gatewayUpdate = true
			}
			continue
		}
		differs, err := crdDiffers(ctx, c, w, &have)
		if err != nil {
			return nil, err
		}
		if differs {
			p.Update = append(p.Update, name)
		}
	}
	return p, nil
}

// crdDiffers applies w with dryRun=All and compares the spec the server would store with the spec it has. The
// server's defaults are in both, so only a real change counts.
func crdDiffers(ctx context.Context, c client.Client, w *unstructured.Unstructured, have *apiextensionsv1.CustomResourceDefinition) (bool, error) {
	dry := w.DeepCopy()
	if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(dry), client.FieldOwner(cliFieldManager), client.ForceOwnership, client.DryRunAll); err != nil {
		return false, fmt.Errorf("dry-run apply of CRD %s: %w", w.GetName(), err)
	}
	a, err := normalize(dry.Object["spec"])
	if err != nil {
		return false, err
	}
	b, err := normalize(have.Spec)
	if err != nil {
		return false, err
	}
	return !reflect.DeepEqual(a, b), nil
}

// normalize turns v into plain JSON values, so a typed spec and an unstructured one compare equal (numbers become
// float64 on both sides).
func normalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	return out, json.Unmarshal(b, &out)
}

// apply applies the CRDs to create, the CRDs to update when update is true, and the bundle's other objects as
// the plan says, with server-side apply (field manager ambit-cli, forced: the person confirmed the change).
func (p *crdPlan) apply(ctx context.Context, c client.Client, update bool, dryRun bool) error {
	want := map[string]bool{}
	for _, n := range p.Create {
		want[n] = true
	}
	if update {
		for _, n := range p.Update {
			want[n] = true
		}
	}
	opts := []client.ApplyOption{client.FieldOwner(cliFieldManager), client.ForceOwnership}
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	for _, w := range p.objs {
		if w.GetKind() == "CustomResourceDefinition" {
			if !want[w.GetName()] {
				continue
			}
		} else if !p.missing[w.GetKind()+"/"+w.GetName()] && !p.gatewayCreate && !(update && p.gatewayUpdate) {
			continue
		}
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(w.DeepCopy()), opts...); err != nil {
			return fmt.Errorf("apply %s %s: %w", w.GetKind(), w.GetName(), err)
		}
	}
	return nil
}

// summary is one line, e.g. "31 created, 3 skipped (newer on the cluster: gateways.gateway.networking.k8s.io ...)".
func (p *crdPlan) summary(updated bool) string {
	var parts []string
	if n := len(p.Create); n > 0 {
		parts = append(parts, fmt.Sprintf("%d created", n))
	}
	if n := len(p.Update); n > 0 && updated {
		parts = append(parts, fmt.Sprintf("%d updated", n))
	}
	if n := len(p.Newer); n > 0 {
		parts = append(parts, fmt.Sprintf("%d left alone (newer on the cluster)", n))
	}
	if len(parts) == 0 {
		return "all up to date"
	}
	return strings.Join(parts, ", ")
}

func semverLess(a, b string) bool {
	va, err1 := utilversion.ParseSemantic(a)
	vb, err2 := utilversion.ParseSemantic(b)
	return err1 == nil && err2 == nil && va.LessThan(vb)
}
