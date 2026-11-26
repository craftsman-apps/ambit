package cli

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// oauthRig is a cluster whose chart has the token policy (OAuth on), or not.
func oauthRig(t *testing.T, oauthOn bool, tokenClauses ...any) *installRig {
	t.Helper()
	r := newInstallRig(t)
	var objs []client.Object
	if oauthOn {
		p := policyObject("ambit-system", oauthTokenPolicy)
		_ = unstructured.SetNestedSlice(p.Object, append([]any{"jwt gate"}, tokenClauses...), "spec", "traffic", "authorization", "policy", "matchExpressions")
		objs = append(objs, p)
	}
	// a cluster that knows AgentgatewayPolicy as an unstructured kind (the CLI needs no Go types of the CRD); the scheme of the other tests is not touched
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	sc.AddKnownTypeWithName(agentgatewayPolicy, &unstructured.Unstructured{})
	r.ctrl = crfake.NewClientBuilder().WithScheme(sc).WithRESTMapper(testrestmapper.TestOnlyStaticRESTMapper(sc)).WithObjects(objs...).Build()
	return r
}

func getBlock(t *testing.T, r *installRig) (*unstructured.Unstructured, bool) {
	t.Helper()
	o := policyObject("ambit-system", oauthBlockPolicy)
	err := r.ctrl.Get(context.Background(), client.ObjectKeyFromObject(o), o)
	return o, err == nil
}

// N1: the block is a policy of its own that the chart does not render, so a helm upgrade or Argo CD self-heal does not undo it.
func TestOAuthBlockOnMakesAPolicyOfItsOwn(t *testing.T) {
	r := oauthRig(t, true)
	code, s := r.run("oauth", "block", "on", "--yes")
	if code != 0 || !strings.Contains(s, "OAuth tokens are blocked") {
		t.Fatalf("exit %d\n%s", code, s)
	}
	o, ok := getBlock(t, r)
	if !ok {
		t.Fatal("no policy was made")
	}
	refs, _, _ := unstructured.NestedSlice(o.Object, "spec", "targetRefs")
	exprs, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "traffic", "authorization", "policy", "matchExpressions")
	action, _, _ := unstructured.NestedString(o.Object, "spec", "traffic", "authorization", "action")
	if len(refs) != 1 || refs[0].(map[string]any)["name"] != "ambit-mcp-public" || refs[0].(map[string]any)["kind"] != "HTTPRoute" ||
		action != "Require" || len(exprs) != 1 || exprs[0] != "!has(jwt.iss)" {
		t.Fatalf("the policy: refs %v action %s exprs %v", refs, action, exprs)
	}
	// nothing that GitOps tools track: no instance label, no Helm ownership, no owner reference
	for k := range o.GetLabels() {
		if strings.HasPrefix(k, "app.kubernetes.io/") || strings.HasPrefix(k, "helm.sh/") {
			t.Errorf("the policy has the label %s, which Helm or Argo CD may track", k)
		}
	}
	if len(o.GetOwnerReferences()) != 0 || o.GetAnnotations()["meta.helm.sh/release-name"] != "" {
		t.Error("the policy has an owner")
	}
	// a second call changes nothing and says so
	code, s = r.run("oauth", "block", "on", "--yes")
	if code != 0 || !strings.Contains(s, "already blocked") {
		t.Fatalf("second call: exit %d\n%s", code, s)
	}
}

func TestOAuthBlockOnAsksFirst(t *testing.T) {
	r := oauthRig(t, true)
	if code, s := r.run("oauth", "block", "on"); code == 0 || !strings.Contains(s, "--yes") {
		t.Fatalf("without a terminal and without --yes: exit %d\n%s", code, s)
	}
	if _, ok := getBlock(t, r); ok {
		t.Fatal("the policy was made without a confirmation")
	}
	r.tty, r.stdin = true, "n\n"
	if code, _ := r.run("oauth", "block", "on"); code == 0 {
		t.Fatal("an answer of n went through")
	}
	if _, ok := getBlock(t, r); ok {
		t.Fatal("the policy was made after an answer of n")
	}
	r.stdin = "y\n"
	if code, s := r.run("oauth", "block", "on"); code != 0 {
		t.Fatalf("an answer of y: exit %d\n%s", code, s)
	}
	if _, ok := getBlock(t, r); !ok {
		t.Fatal("no policy after y")
	}
}

func TestOAuthBlockNeedsOAuthOn(t *testing.T) {
	r := oauthRig(t, false)
	code, s := r.run("oauth", "block", "on", "--yes")
	if code == 0 || !strings.Contains(s, "OAuth sign-in is not turned on") {
		t.Fatalf("exit %d\n%s", code, s)
	}
	if _, ok := getBlock(t, r); ok {
		t.Fatal("a policy was made without OAuth")
	}
}

func TestOAuthBlockOffLiftsIt(t *testing.T) {
	r := oauthRig(t, true)
	r.run("oauth", "block", "on", "--yes")
	if code, s := r.run("oauth", "block", "off"); code == 0 || !strings.Contains(s, "--yes") {
		t.Fatalf("off without confirmation: exit %d\n%s", code, s)
	}
	if _, ok := getBlock(t, r); !ok {
		t.Fatal("the block was lifted without a confirmation")
	}
	code, s := r.run("oauth", "block", "off", "--yes")
	if code != 0 || !strings.Contains(s, "The block is lifted") {
		t.Fatalf("exit %d\n%s", code, s)
	}
	if _, ok := getBlock(t, r); ok {
		t.Fatal("the policy stays")
	}
	// nothing to lift: a message that names the other ways to block, and no error
	code, s = r.run("oauth", "block", "off", "--yes")
	if code != 0 || !strings.Contains(s, "no block made by this command") || !strings.Contains(s, "oauth.emergencyBlock") {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

func TestOAuthBlockStatus(t *testing.T) {
	r := oauthRig(t, true)
	if _, s := r.run("oauth", "block", "status"); !strings.Contains(s, "not blocked") {
		t.Fatalf("not blocked:\n%s", s)
	}
	r.run("oauth", "block", "on", "--yes")
	if _, s := r.run("oauth", "block", "status"); !strings.Contains(s, "BLOCKED by the policy "+oauthBlockPolicy) {
		t.Fatalf("blocked by the policy:\n%s", s)
	}
	// the old way (a clause in the chart's policy) is named, with its weakness
	r2 := oauthRig(t, true, "false")
	if _, s := r2.run("oauth", "block", "status"); !strings.Contains(s, "clause `false`") || !strings.Contains(s, "Helm or Argo CD may remove it") {
		t.Fatalf("blocked by a clause:\n%s", s)
	}
	r3 := oauthRig(t, false)
	if _, s := r3.run("oauth", "block", "status"); !strings.Contains(s, "not turned on") {
		t.Fatalf("OAuth off:\n%s", s)
	}
}
