package controller

import (
	"slices"
	"testing"

	"github.com/craftsman-apps/ambit/internal/identity"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestOffboardedUsersAreRefusedOnBothRoutes(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: "ambit-system", UID: "uid-cm"},
		Data: map[string]string{"emails": `["User02@example.com", "user01@example.com", "x\" || true || \"@evil"]`}}
	ok, rejected := OffboardedEmails(cm)
	if len(ok) != 2 || ok[0] != "user01@example.com" || ok[1] != "user02@example.com" || len(rejected) != 1 {
		t.Fatalf("ok %v rejected %v", ok, rejected)
	}
	p := offboardedPolicy(cm, ok, []string{"ambit-mcp", "ambit-llm"})
	e, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
	action, _, _ := unstructured.NestedString(p.Object, "spec", "traffic", "authorization", "action")
	refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	if action != "Require" || e[0] != `!(`+identity.Principal(true)+` in ["user01@example.com", "user02@example.com"])` || len(refs) != 2 {
		t.Fatalf("%s %v %v", action, e, refs)
	}
	if empty := offboardedPolicy(cm, nil, []string{"ambit-mcp"}); true {
		e, _, _ := unstructured.NestedStringSlice(empty.Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
		if e[0] != "true" {
			t.Fatalf("empty list must deny nobody: %v", e)
		}
	}
}

// The public Gateway's route is refused for deactivated people like the private routes (frontDoor.enabled).
func TestOffboardedUsersAreRefusedOnThePublicRouteToo(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: "ambit-system"}, Data: map[string]string{"emails": `["gone@example.com"]`}}
	for gateway, want := range map[string][]string{"": {"ambit-mcp", "ambit-llm"}, "ambit-public": {"ambit-mcp", "ambit-llm", "ambit-mcp-public"}} {
		r := &Reconciler{Live: fake.NewClientBuilder().WithObjects(cm.DeepCopy()).Build(), Namespace: "ambit-system", Route: RouteRef{Name: "ambit-llm"}, PublicGateway: gateway}
		p, err := r.offboarding(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
		var got []string
		for _, ref := range refs {
			got = append(got, ref.(map[string]any)["name"].(string))
		}
		if !slices.Equal(got, want) {
			t.Errorf("public gateway %q: routes %v, want %v", gateway, got, want)
		}
	}
}
