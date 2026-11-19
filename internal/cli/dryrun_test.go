package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// 3.2 dry run: no create, update or Secret write reaches the cluster; objects of kinds the cluster does not know
// are listed as not validated; no question is asked.
func TestInstallDryRunWritesNothing(t *testing.T) {
	r := newInstallRig(t)
	applied := 0
	r.ctrl = fakeCtrl(&applied)
	os.WriteFile(r.path("secret.txt"), []byte("client-secret-value-123456"), 0o600)
	r.helm.manifest = map[string]string{
		"ambit-crds": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: crds-info}\n",
		"ambit": `apiVersion: apps/v1
kind: Deployment
metadata: {name: ambit-console}
spec:
  selector: {matchLabels: {app: c}}
  template: {metadata: {labels: {app: c}}, spec: {containers: [{name: c, image: x}]}}
---
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayPolicy
metadata: {name: ambit-llm}
spec: {}
`}
	code, s := r.run("install", "-f", r.path("values.yaml"), "--dry-run", "--secret-file", "ambit-sso/clientSecret="+r.path("secret.txt"))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, s)
	}
	for _, want := range []string{
		"Installing Ambit 0.5.0 into context prod-eu (https://1.2.3.4:6443), namespace ambit-system (dry run)",
		"Would apply 28 dependency CRDs",
		"Would install ambit-crds 0.5.0 (1 objects) and ambit 0.5.0 (2 objects) in ambit-system",
		"validated by the API server: 2; not validated (CRD not installed yet): 1",
		"the namespace ambit-system does not exist yet",
		"AgentgatewayPolicy ambit-llm: not validated (CRD not installed yet)",
		"Deployment ambit-console: validated",
		"Would create or update Secret ambit-sso key clientSecret, from " + r.path("secret.txt"),
		"dry run: nothing was changed",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "Install? [y/N]") || strings.Contains(s, "client-secret-value") {
		t.Fatalf("asked, or printed the secret:\n%s", s)
	}
	if r.wrote() || applied != 0 {
		t.Fatalf("the dry run wrote: %d applies, %v", applied, r.kube.Actions())
	}
	var crds apiextensionsv1.CustomResourceDefinitionList
	if err := r.ctrl.List(context.Background(), &crds); err != nil || len(crds.Items) != 0 {
		t.Fatalf("the dry run stored %d CRDs, %v", len(crds.Items), err)
	}
}
