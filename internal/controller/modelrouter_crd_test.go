package controller

import (
	"slices"
	"testing"
	"time"

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
		"vsr-refused":   {Default: "g", Classifier: v1.RouterClassifier{Type: "VSR"}},
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
	llm := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "smart", Namespace: ns},
		Spec: v1.ModelRouterSpec{Default: "gemma", Classifier: v1.RouterClassifier{Type: "LLM", LLM: &v1.RouterLLM{Model: "gemma-local"},
			Categories: []v1.RouterCategory{{Name: "coding", Description: "writing, fixing or explaining program code"}}},
			Routes: []v1.RouterRoute{{Model: "sonnet", Category: "coding"}}}}
	if err := envClient.Create(ctx, llm); err != nil {
		t.Fatalf("valid LLM router refused: %v", err)
	}
	m := &v1.RouterLLM{Model: "m"}
	for name, spec := range map[string]v1.ModelRouterSpec{
		"llm-without-model": {Default: "g", Classifier: v1.RouterClassifier{Type: "LLM"}},
		"other-reserved":    {Default: "g", Classifier: v1.RouterClassifier{Type: "LLM", LLM: m, Categories: []v1.RouterCategory{{Name: "other", Description: "x"}}}},
		"empty-description": {Default: "g", Classifier: v1.RouterClassifier{Type: "LLM", LLM: m, Categories: []v1.RouterCategory{{Name: "coding"}}}},
		"zero-rate":         {Default: "g", Classifier: v1.RouterClassifier{Type: "LLM", LLM: m, RatePerMinute: ptr.To[int32](0)}},
		"huge-maxbytes":     {Default: "g", Classifier: v1.RouterClassifier{Type: "LLM", LLM: m, MaxBytes: ptr.To[int64](3 << 20)}},
		"too-many-cats":     {Default: "g", Classifier: v1.RouterClassifier{Type: "LLM", LLM: m, Categories: slices.Repeat([]v1.RouterCategory{{Name: "a", Description: "a"}}, 17)}},
		// longer than this would outlast ambit-router's 25 s drain: rollouts would cut classifications (final review)
		"timeout-over-10s": {Default: "g", Classifier: v1.RouterClassifier{Type: "LLM", LLM: m, Timeout: &metav1.Duration{Duration: 11 * time.Second}}},
	} {
		o := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
		if err := envClient.Create(ctx, o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	ten := &v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "ten-seconds", Namespace: ns},
		Spec: v1.ModelRouterSpec{Default: "g", Classifier: v1.RouterClassifier{Type: "LLM", LLM: m, Timeout: &metav1.Duration{Duration: 10 * time.Second}}}}
	if err := envClient.Create(ctx, ten); err != nil {
		t.Fatalf("a 10 s timeout is allowed: %v", err)
	}
	var c v1.RouterClassifier
	if c.TimeoutOrDefault() != 1500*time.Millisecond || c.MaxBytesOrDefault() != 8000 || c.RateOrDefault() != 600 {
		t.Fatal("defaults")
	}
	if (&v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto"}}).WireName() != "auto" ||
		(&v1.ModelRouter{ObjectMeta: metav1.ObjectMeta{Name: "auto"}, Spec: v1.ModelRouterSpec{Model: "smart"}}).WireName() != "smart" {
		t.Fatal("WireName")
	}
}
