package controller

import (
	"slices"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestModelRouterCRDRules(t *testing.T) {
	needEnv(t)
	ns := "ambit-router-crd"
	namespace(t, ns)
	ctx := t.Context()
	ok := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto", Namespace: ns},
		Spec: v1.ModelRouterSpec{Default: "gemma", Classifier: v1.RouterClassifier{Type: "Rules"},
			Routes: []v1.RouterRoute{{Model: "sonnet", Keywords: []string{"python", `say "hi"`}}, {Model: "pro", MinBytes: ptr.To[int64](2000)},
				{Model: "sonnet", Category: "coding"}}}}
	if err := envClient.Create(ctx, ok); err != nil {
		t.Fatalf("valid router refused: %v", err)
	}
	var got v1.ModelRouter
	_ = envClient.Get(ctx, client.ObjectKeyFromObject(ok), &got)
	if got.Spec.Classifier.Type != "Rules" {
		t.Fatalf("type %q", got.Spec.Classifier.Type)
	}
	def := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: ns}, Spec: v1.ModelRouterSpec{Default: "gemma"}}
	if err := envClient.Create(ctx, def); err != nil {
		t.Fatal(err)
	}
	_ = envClient.Get(ctx, client.ObjectKeyFromObject(def), &got)
	if got.Spec.Classifier.Type != "None" {
		t.Fatalf("default type %q, want None", got.Spec.Classifier.Type)
	}
	for name, spec := range map[string]v1.ModelRouterSpec{
		"no-default":    {},
		"vsr-not-yet":   {Default: "g", Classifier: v1.RouterClassifier{Type: "VSR"}},
		"empty-route":   {Default: "g", Routes: []v1.RouterRoute{{Model: "x"}}},
		"bad-category":  {Default: "g", Routes: []v1.RouterRoute{{Model: "x", Category: "Coding!"}}},
		"zero-bytes":    {Default: "g", Routes: []v1.RouterRoute{{Model: "x", MinBytes: ptr.To[int64](0)}}},
		"non-ascii-kw":  {Default: "g", Routes: []v1.RouterRoute{{Model: "x", Keywords: []string{"größe"}}}},
		"too-many":      {Default: "g", Routes: slices.Repeat([]v1.RouterRoute{{Model: "x", Keywords: []string{"a"}}}, 17)},
		"bad-wire-name": {Default: "g", Model: "has space"},
	} {
		o := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
		if err := envClient.Create(ctx, o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if (&v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto"}}).WireName() != "auto" ||
		(&v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto"}, Spec: v1.ModelRouterSpec{Model: "smart"}}).WireName() != "smart" {
		t.Fatal("WireName")
	}
}
