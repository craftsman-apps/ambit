package controller

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errRulesRevoked ends the wait of a change in progress: an AccessRule it compiled was deleted or narrowed meanwhile.
var errRulesRevoked = errors.New("an access rule was removed or narrowed while a change waited for agentgateway")

// revocationCheck is how often a waiting change looks at the AccessRules (#430). Short: the removal of access must not
// wait for the settle time (3 s) of each step of the change in progress.
const revocationCheck = 250 * time.Millisecond

// narrows reports whether next takes something away that prev allowed: a group, a person, a model, a server or a
// tool is gone, allSignedIn is off, or the argument limits of a server differ (a limit may be stricter; a restart of
// the change costs only time). A rule that also gains something counts as narrowing.
func narrows(prev, next v1.AccessRuleSpec) bool {
	if prev.AllSignedIn && !next.AllSignedIn {
		return true
	}
	for _, g := range prev.Groups {
		if !slices.Contains(next.Groups, g) {
			return true
		}
	}
	for _, u := range prev.Users {
		if !slices.Contains(next.Users, u) {
			return true
		}
	}
	for _, m := range prev.Models {
		if !slices.Contains(next.Models, m) {
			return true
		}
	}
	for _, ps := range prev.Servers {
		if !covered(ps, next.Servers) {
			return true
		}
	}
	return false
}

// covered: some entry of next names the same server with the same argument limits and allows at least the tools of ps
// (a rule may name a server in several entries, so each entry is compared with every entry of next, not with the sum).
func covered(ps v1.ServerAccess, next []v1.ServerAccess) bool {
	for _, s := range next {
		if s.Name != ps.Name || !reflect.DeepEqual(normArgs(ps.Arguments), normArgs(s.Arguments)) {
			continue
		}
		if len(s.Tools) == 0 { // every tool
			return true
		}
		if len(ps.Tools) == 0 {
			continue
		}
		if !slices.ContainsFunc(ps.Tools, func(t string) bool { return !slices.Contains(s.Tools, t) }) {
			return true
		}
	}
	return false
}

// normArgs: the limits as a set (the order of the rules and of their lists does not matter). A changed limit is not
// compared further: it counts as narrowing (a restart of the change costs only time).
func normArgs(in []v1.ArgumentRule) []v1.ArgumentRule {
	out := make([]v1.ArgumentRule, len(in))
	for i, a := range in {
		a.OneOf, a.NotContains = slices.Sorted(slices.Values(a.OneOf)), slices.Sorted(slices.Values(a.NotContains))
		out[i] = a
	}
	key := func(a v1.ArgumentRule) string {
		return strings.Join([]string{a.Name, a.Min, a.Max, strings.Join(a.OneOf, "\x01"), strings.Join(a.NotContains, "\x01")}, "\x00")
	}
	slices.SortFunc(out, func(a, b v1.ArgumentRule) int { return cmp.Compare(key(a), key(b)) })
	if len(out) == 0 {
		return nil
	}
	return out
}

// rulesRevoked reports whether an AccessRule that the change in progress compiled has been deleted or narrowed since
// (read from the API server). A grant (a new or wider rule) is not a revocation and keeps the order and the waits of
// the change.
func (r *Reconciler) rulesRevoked(ctx context.Context) (bool, error) {
	if !r.watchRevocations {
		return false, nil
	}
	var now v1.AccessRuleList
	if err := r.Live.List(ctx, &now, client.InNamespace(r.Namespace)); err != nil {
		return false, err
	}
	cur := map[string]v1.AccessRuleSpec{}
	for _, ar := range now.Items {
		cur[ar.Name] = ar.Spec
	}
	for _, prev := range r.passRules {
		next, ok := cur[prev.Name]
		if !ok || narrows(prev.Spec, next) {
			return true, nil
		}
	}
	return false, nil
}

// sleepWatching is sleep for the waits of the gate: it looks for a revoked rule every revocationCheck and ends with
// errRulesRevoked, so the next pass writes the removal at once (#430).
func (r *Reconciler) sleepWatching(ctx context.Context, d time.Duration) error {
	if !r.watchRevocations {
		return r.sleep(ctx, d)
	}
	for d > 0 {
		step := min(d, revocationCheck)
		if err := r.sleep(ctx, step); err != nil {
			return err
		}
		d -= step
		if revoked, err := r.rulesRevoked(ctx); err != nil || revoked {
			if err != nil {
				return err
			}
			return errRulesRevoked
		}
	}
	return nil
}
