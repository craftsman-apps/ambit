package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNarrows(t *testing.T) {
	base := v1.AccessRuleSpec{Groups: []string{"eng", "ops"}, Users: []string{"a@x.com"}, Models: []string{"m1", "m2"},
		Servers: []v1.ServerAccess{{Name: "time", Tools: []string{"t1", "t2"}}, {Name: "all"}}}
	with := func(f func(*v1.AccessRuleSpec)) v1.AccessRuleSpec {
		s := *base.DeepCopy()
		f(&s)
		return s
	}
	for _, c := range []struct {
		name string
		next v1.AccessRuleSpec
		want bool
	}{
		{"unchanged", base, false},
		{"a group removed", with(func(s *v1.AccessRuleSpec) { s.Groups = []string{"eng"} }), true},
		{"a person removed", with(func(s *v1.AccessRuleSpec) { s.Users = nil }), true},
		{"a model removed", with(func(s *v1.AccessRuleSpec) { s.Models = []string{"m1"} }), true},
		{"a server removed", with(func(s *v1.AccessRuleSpec) { s.Servers = s.Servers[:1] }), true},
		{"a tool removed", with(func(s *v1.AccessRuleSpec) { s.Servers[0].Tools = []string{"t1"} }), true},
		{"every tool before, a list now", with(func(s *v1.AccessRuleSpec) { s.Servers[1].Tools = []string{"t1"} }), true},
		{"argument limits changed", with(func(s *v1.AccessRuleSpec) {
			s.Servers[0].Arguments = []v1.ArgumentRule{{Name: "n", OneOf: []string{"x"}}}
		}), true},
		{"a group added", with(func(s *v1.AccessRuleSpec) { s.Groups = append(s.Groups, "new") }), false},
		{"a model added", with(func(s *v1.AccessRuleSpec) { s.Models = append(s.Models, "m3") }), false},
		{"a list of tools becomes every tool", with(func(s *v1.AccessRuleSpec) { s.Servers[0].Tools = nil }), false},
		{"a tool added", with(func(s *v1.AccessRuleSpec) { s.Servers[0].Tools = append(s.Servers[0].Tools, "t3") }), false},
		{"allSignedIn turned on", with(func(s *v1.AccessRuleSpec) { s.AllSignedIn = true }), false},
		{"one group swapped for another", with(func(s *v1.AccessRuleSpec) { s.Groups = []string{"eng", "new"} }), true},
	} {
		if got := narrows(base, c.next); got != c.want {
			t.Errorf("%s: narrows = %v, want %v", c.name, got, c.want)
		}
	}
	// A rule that names one server in several entries does not narrow itself.
	split := v1.AccessRuleSpec{Servers: []v1.ServerAccess{{Name: "time", Tools: []string{"t1"}}, {Name: "time", Tools: []string{"t2"}}}}
	if narrows(split, split) {
		t.Error("a rule with two entries for one server narrows itself")
	}
	// Merging them keeps every tool; splitting one entry in two is treated as narrowing (conservative).
	merged := v1.AccessRuleSpec{Servers: []v1.ServerAccess{{Name: "time", Tools: []string{"t1", "t2"}}}}
	if narrows(split, merged) {
		t.Error("merging two entries counted as narrowing")
	}
	if !narrows(merged, split) {
		t.Error("splitting an entry was not treated as narrowing")
	}
	if !narrows(v1.AccessRuleSpec{AllSignedIn: true}, v1.AccessRuleSpec{Groups: []string{"eng"}}) {
		t.Error("allSignedIn turned off was not a narrowing")
	}
}

// #430: the wait of a change in progress ends at once, not after the settle time and the timeout, when a rule that it
// compiled is deleted or narrowed; a rule that is added or widened does not end it.
func TestGateEndsWhenARuleIsRevoked(t *testing.T) {
	setGroups := func(t *testing.T, ctx context.Context, r *Reconciler, groups ...string) {
		t.Helper()
		var ar v1.AccessRule
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "ambit-system", Name: "eng"}, &ar); err != nil {
			t.Fatal(err)
		}
		ar.Spec.Groups = groups
		if err := r.Client.Update(ctx, &ar); err != nil {
			t.Fatal(err)
		}
	}
	rule := func(groups ...string) *v1.AccessRule {
		return &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "ambit-system"}, Spec: v1.AccessRuleSpec{Groups: groups, Models: []string{"m"}}}
	}
	for _, c := range []struct {
		name   string
		change func(context.Context, *Reconciler)
		ends   bool
	}{
		{"deleted", func(ctx context.Context, r *Reconciler) { _ = r.Client.Delete(ctx, rule("a", "b")) }, true},
		{"narrowed", func(ctx context.Context, r *Reconciler) { setGroups(t, ctx, r, "a") }, true},
		{"widened", func(ctx context.Context, r *Reconciler) { setGroups(t, ctx, r, "a", "b", "c") }, false},
		{"another rule added", func(ctx context.Context, r *Reconciler) {
			o := rule("z")
			o.Name = "other"
			_ = r.Client.Create(ctx, o)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, clock := gateReconciler(gatedPolicy(1)) // no status: the gate waits for agentgateway
			sch := runtime.NewScheme()
			_ = corev1.AddToScheme(sch)
			_ = v1.AddToScheme(sch)
			cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(rule("a", "b")).
				WithIndex(&corev1.Event{}, "reason", func(o client.Object) []string { return []string{o.(*corev1.Event).Reason} }).Build()
			r.Client, r.Live = cl, cl
			// the policy lives in an unstructured fake: put it in this one too
			_ = cl.Create(t.Context(), gatedPolicy(1))
			prev := rule("a", "b")
			r.passRules, r.watchRevocations = []v1.AccessRule{*prev}, true
			inner := r.Activation.Sleep
			changed := false
			r.Activation.Sleep = func(ctx context.Context, d time.Duration) error {
				if !changed && clock.Sub(gateStart) >= time.Second {
					changed = true
					c.change(ctx, r)
				}
				return inner(ctx, d)
			}
			err := r.gate(t.Context(), []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 1}}, nil, 120*time.Second)
			elapsed := clock.Sub(gateStart)
			if c.ends {
				if !errors.Is(err, errRulesRevoked) {
					t.Fatalf("error %v, want errRulesRevoked", err)
				}
				if elapsed > 2*time.Second {
					t.Errorf("ended after %v, want within a second of the change", elapsed)
				}
				return
			}
			if err == nil || errors.Is(err, errRulesRevoked) || elapsed < 120*time.Second {
				t.Errorf("a grant ended the wait: %v after %v", err, elapsed)
			}
		})
	}
}

// Without watching (no activation) a revoked rule does not end the wait.
func TestGateDoesNotWatchWithoutTheFlag(t *testing.T) {
	r, clock := gateReconciler(gatedPolicy(1))
	r.passRules, r.watchRevocations = []v1.AccessRule{{ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: "ambit-system"}}}, false
	err := r.gate(t.Context(), []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 1}}, nil, 10*time.Second)
	if err == nil || errors.Is(err, errRulesRevoked) || clock.Sub(gateStart) < 10*time.Second {
		t.Errorf("err %v after %v", err, clock.Sub(gateStart))
	}
}

// The same limits in another order are not a change.
func TestNarrowsIgnoresTheOrderOfArgumentLimits(t *testing.T) {
	mk := func(args ...v1.ArgumentRule) v1.AccessRuleSpec {
		return v1.AccessRuleSpec{Servers: []v1.ServerAccess{{Name: "db", Tools: []string{"q"}, Arguments: args}}}
	}
	a := mk(v1.ArgumentRule{Name: "b", OneOf: []string{"x", "y"}}, v1.ArgumentRule{Name: "a", Max: "5", Min: "1"})
	b := mk(v1.ArgumentRule{Name: "a", Max: "5", Min: "1"}, v1.ArgumentRule{Name: "b", OneOf: []string{"y", "x"}})
	if narrows(a, b) || narrows(b, a) {
		t.Error("the order of the limits counted as a change")
	}
	if !narrows(a, mk(v1.ArgumentRule{Name: "a", Max: "9", Min: "1"}, v1.ArgumentRule{Name: "b", OneOf: []string{"x", "y"}})) {
		t.Error("a changed limit did not count (conservative)")
	}
}

// A wait that ends because a rule was revoked still reports a NACK that arrived since the last poll: the next pass takes
// a new baseline and would not see it.
func TestRevokedWaitStillReportsANack(t *testing.T) {
	r, clock := gateReconciler(gatedPolicy(1))
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = v1.AddToScheme(sch)
	rule := &v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "ambit-system"}, Spec: v1.AccessRuleSpec{Groups: []string{"a"}}}
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(rule).
		WithIndex(&corev1.Event{}, "reason", func(o client.Object) []string { return []string{o.(*corev1.Event).Reason} }).Build()
	r.Client, r.Live = cl, cl
	_ = cl.Create(t.Context(), gatedPolicy(1))
	r.passRules, r.watchRevocations = []v1.AccessRule{*rule.DeepCopy()}, true
	inner := r.Activation.Sleep
	done := false
	r.Activation.Sleep = func(ctx context.Context, d time.Duration) error {
		if !done && clock.Sub(gateStart) >= time.Second {
			done = true
			_ = cl.Delete(ctx, rule)
			_ = cl.Create(ctx, nack(gateStart.Add(time.Second)))
		}
		return inner(ctx, d)
	}
	err := r.gate(t.Context(), []gated{{kind: "AgentgatewayPolicy", name: AccessLogPolicy, generation: 1}}, nackSeen{}, 120*time.Second)
	var st *stalledError
	if errors.Is(err, errRulesRevoked) || !errors.As(err, &st) {
		t.Fatalf("error %v, want the NACK as a stalled change", err)
	}
}
