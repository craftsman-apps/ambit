package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestOffboardedUsersAreDeniedOnBothRoutes(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: "ambit-system", UID: "uid-cm"},
		Data: map[string]string{"emails": `["Bob@Acme.test", "alice@acme.test", "x\" || true || \"@evil"]`}}
	ok, rejected := OffboardedEmails(cm)
	if len(ok) != 2 || ok[0] != "alice@acme.test" || ok[1] != "bob@acme.test" || len(rejected) != 1 {
		t.Fatalf("ok %v rejected %v", ok, rejected)
	}
	p := offboardedPolicy(cm, ok, []string{"ambit-mcp", "ambit-llm"})
	e, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
	action, _, _ := unstructured.NestedString(p.Object, "spec", "traffic", "authorization", "action")
	refs, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	if action != "Deny" || e[0] != `jwt.email in ["alice@acme.test", "bob@acme.test"]` || len(refs) != 2 {
		t.Fatalf("%s %v %v", action, e, refs)
	}
	if empty := offboardedPolicy(cm, nil, []string{"ambit-mcp"}); true {
		e, _, _ := unstructured.NestedStringSlice(empty.Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
		if e[0] != "false" {
			t.Fatalf("empty list must deny nobody: %v", e)
		}
	}
}
