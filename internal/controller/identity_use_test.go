package controller

import (
	"strings"
	"testing"

	"github.com/craftsman-apps/ambit/internal/identity"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// No generated expression may read jwt.email or jwt.groups directly: identity comes from the shared fragments.
func TestGeneratorsUseRequestIdentity(t *testing.T) {
	g := identity.PrincipalGroups
	p := identity.Principal(true)
	cases := map[string]string{
		"CEL":            CEL(Access{Groups: []string{"eng"}}),
		"member":         member("eng"),
		"spendUser":      spendUser(),
		"usageCondition": usageCondition([]string{"eng"}),
	}
	for name, got := range cases {
		if strings.Contains(strings.ReplaceAll(got, g, ""), "jwt.groups") || strings.Contains(strings.ReplaceAll(got, p, ""), "jwt.email") {
			t.Errorf("%s reads the JWT directly: %s", name, got)
		}
	}
	if !strings.Contains(CEL(Access{Groups: []string{"eng"}}), g) || spendUser() != p {
		t.Error("CEL or spendUser does not use the identity fragments")
	}
	if CEL(Access{AllSignedIn: true}) != "true" {
		t.Error("allSignedIn must stay true; the auth Require rule guards it")
	}
}

func TestOffboardedIsAFailClosedRequireOnThePrincipal(t *testing.T) {
	cm := &corev1.ConfigMap{}
	cm.Namespace = "ambit-system"
	o := offboardedPolicy(cm, []string{"alice@acme.com"}, []string{"ambit-mcp", "ambit-llm"})
	a, _, _ := unstructured.NestedMap(o.Object, "spec", "traffic", "authorization")
	if a["action"] != "Require" {
		t.Fatalf("action %v", a["action"])
	}
	exprs, _, _ := unstructured.NestedStringSlice(a, "policy", "matchExpressions")
	if want := `!(` + identity.Principal(true) + ` in ["alice@acme.com"])`; len(exprs) != 1 || exprs[0] != want {
		t.Fatalf("got %v, want %s", exprs, want)
	}
	empty := offboardedPolicy(cm, nil, []string{"ambit-llm"})
	exprs, _, _ = unstructured.NestedStringSlice(empty.Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
	if exprs[0] != "true" {
		t.Fatalf("empty list must allow: %v", exprs)
	}
}

func TestOffboardedEmailsUseTheSharedEmailRule(t *testing.T) {
	cm := &corev1.ConfigMap{Data: map[string]string{"emails": `[" Alice@Acme.com ","bob o@x.com","o'brien@acme.com"]`}}
	ok, rejected := OffboardedEmails(cm)
	if strings.Join(ok, ",") != "alice@acme.com,o'brien@acme.com" || len(rejected) != 1 {
		t.Fatalf("ok %v rejected %v", ok, rejected)
	}
}
