package cli

import (
	"context"
	"strings"
	"testing"
	"time"

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
	was := oauthBlockWait
	oauthBlockWait = 0 // the fake cluster has no gateway that reports the policy accepted
	t.Cleanup(func() { oauthBlockWait = was })
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
	// no gateway in the fake cluster: the policy is made, and the command fails: nobody may rely on a block that no gateway has accepted
	if code == 0 || !strings.Contains(s, "is created") || !strings.Contains(s, "no gateway has reported it accepted") || strings.Contains(s, "and the gateway has accepted it") {
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
	// a second call changes nothing; it does not claim a block that the gateway has not accepted
	code, s = r.run("oauth", "block", "on", "--yes")
	if code == 0 || !strings.Contains(s, "exists in namespace") || !strings.Contains(s, "no gateway has reported it accepted") || strings.Contains(s, "already blocked") {
		t.Fatalf("second call while pending: exit %d\n%s", code, s)
	}
	o, _ = getBlock(t, r)
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "True"}}}}, "status", "ancestors")
	if err := r.ctrl.Update(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	code, s = r.run("oauth", "block", "on", "--yes")
	if code != 0 || !strings.Contains(s, "already blocked") {
		t.Fatalf("second call after acceptance: exit %d\n%s", code, s)
	}
}

// When a gateway accepts the new policy in time, `on` says so and exits 0.
func TestOAuthBlockOnSucceedsWhenTheGatewayAccepts(t *testing.T) {
	r := oauthRig(t, true)
	was := oauthBlockWait
	oauthBlockWait = 5 * time.Second
	t.Cleanup(func() { oauthBlockWait = was })
	go func() { // the gateway: reports the policy accepted shortly after it appears
		for i := 0; i < 50; i++ {
			time.Sleep(100 * time.Millisecond)
			if o, ok := getBlockQuiet(r); ok {
				_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "True"}}}}, "status", "ancestors")
				if r.ctrl.Update(context.Background(), o) == nil {
					return
				}
			}
		}
	}()
	code, s := r.run("oauth", "block", "on", "--yes")
	if code != 0 || !strings.Contains(s, "OAuth tokens are blocked") || !strings.Contains(s, "the gateway has accepted it") {
		t.Fatalf("exit %d\n%s", code, s)
	}
}

func getBlockQuiet(r *installRig) (*unstructured.Unstructured, bool) {
	o := policyObject("ambit-system", oauthBlockPolicy)
	err := r.ctrl.Get(context.Background(), client.ObjectKeyFromObject(o), o)
	return o, err == nil
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
	r.run("oauth", "block", "on") // an answer of y makes the policy (the fake cluster has no gateway: the exit code is not the point here)
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

// Lifting needs no token policy of the chart (the block must be removable also when OAuth was turned off meanwhile).
func TestOAuthBlockOffWorksWithoutTheChartPolicy(t *testing.T) {
	r := oauthRig(t, true)
	r.run("oauth", "block", "on", "--yes")
	b, _ := getBlock(t, r)
	r2 := oauthRig(t, false)
	c := blockPolicy("ambit-system")
	c.SetResourceVersion("")
	if err := r2.ctrl.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	_ = b
	if code, s := r2.run("oauth", "block", "off", "--yes"); code != 0 || !strings.Contains(s, "The block is lifted") {
		t.Fatalf("exit %d\n%s", code, s)
	}
	if _, ok := getBlock(t, r2); ok {
		t.Fatal("the policy stays")
	}
}

// A policy of the same name that is not the block is never reported as a block, and never deleted.
func TestOAuthBlockRefusesAPolicyThatIsNotTheBlock(t *testing.T) {
	r := oauthRig(t, true)
	other := policyObject("ambit-system", oauthBlockPolicy)
	_ = unstructured.SetNestedSlice(other.Object, []any{"true"}, "spec", "traffic", "authorization", "policy", "matchExpressions")
	if err := r.ctrl.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"oauth", "block", "on", "--yes"}, {"oauth", "block", "off", "--yes"}} {
		if code, s := r.run(args...); code == 0 || !strings.Contains(s, "is not the block") {
			t.Fatalf("%v: exit %d\n%s", args, code, s)
		}
	}
	if _, ok := getBlock(t, r); !ok {
		t.Fatal("an unrelated policy was deleted")
	}
	if _, s := r.run("oauth", "block", "status"); !strings.Contains(s, "not the block") || strings.Contains(s, "BLOCKED by the policy") {
		t.Fatalf("status of a policy that is not the block:\n%s", s)
	}
}

func TestOAuthBlockStatus(t *testing.T) {
	r := oauthRig(t, true)
	if _, s := r.run("oauth", "block", "status"); !strings.Contains(s, "not blocked") {
		t.Fatalf("not blocked:\n%s", s)
	}
	r.run("oauth", "block", "on", "--yes")
	if _, s := r.run("oauth", "block", "status"); !strings.Contains(s, "has not reported it accepted") || strings.Contains(s, "BLOCKED by the policy") {
		t.Fatalf("made but not accepted:\n%s", s)
	}
	// the gateway reports it accepted
	o, _ := getBlock(t, r)
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "True"}}}}, "status", "ancestors")
	if err := r.ctrl.Update(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, s := r.run("oauth", "block", "status"); !strings.Contains(s, "BLOCKED by the policy "+oauthBlockPolicy) || !strings.Contains(s, "accepted") {
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

// The spec must be exactly the block's: the same rule plus another setting is not the block, and is never deleted.
func TestOAuthBlockIsTheWholeSpec(t *testing.T) {
	r := oauthRig(t, true)
	other := blockPolicy("ambit-system")
	_ = unstructured.SetNestedField(other.Object, "Allow", "spec", "traffic", "authorization", "action") // another action
	if isBlock(other) {
		t.Fatal("a different action was taken for the block")
	}
	extra := blockPolicy("ambit-system")
	_ = unstructured.SetNestedField(extra.Object, map[string]any{"x": "y"}, "spec", "backend", "mcp")
	if isBlock(extra) {
		t.Fatal("the same rule with another setting was taken for the block")
	}
	if !isBlock(blockPolicy("ambit-system")) {
		t.Fatal("the block is not the block")
	}
	if err := r.ctrl.Create(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	if code, s := r.run("oauth", "block", "off", "--yes"); code == 0 || !strings.Contains(s, "is not the block") {
		t.Fatalf("exit %d\n%s", code, s)
	}
	if _, ok := getBlock(t, r); !ok {
		t.Fatal("a policy with other settings was deleted")
	}
}

// A condition about an older version of the policy does not count as accepted.
func TestAcceptedNeedsTheCurrentGeneration(t *testing.T) {
	o := blockPolicy("ambit-system")
	o.SetGeneration(3)
	cond := func(gen int64, attached string) []any {
		c := []any{map[string]any{"type": "Accepted", "status": "True", "observedGeneration": gen}}
		if attached != "" {
			c = append(c, map[string]any{"type": "Attached", "status": attached, "observedGeneration": gen})
		}
		return []any{map[string]any{"conditions": c}}
	}
	for name, tc := range map[string]struct {
		anc  []any
		want bool
	}{"current": {cond(3, ""), true}, "stale": {cond(2, ""), false}, "attached false": {cond(3, "False"), false}, "attached true": {cond(3, "True"), true}, "none": {nil, false}} {
		_ = unstructured.SetNestedSlice(o.Object, tc.anc, "status", "ancestors")
		if tc.anc == nil {
			unstructured.RemoveNestedField(o.Object, "status")
		}
		if got := accepted(o); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}
