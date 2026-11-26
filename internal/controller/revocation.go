package controller

import (
	"context"
	"errors"
	"reflect"
	"slices"
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
		var ns []v1.ServerAccess
		for _, s := range next.Servers {
			if s.Name == ps.Name {
				ns = append(ns, s)
			}
		}
		if len(ns) == 0 {
			return true
		}
		for _, s := range ns {
			if !reflect.DeepEqual(ps.Arguments, s.Arguments) {
				return true
			}
			if len(ps.Tools) == 0 { // every tool before
				if len(s.Tools) > 0 {
					return true
				}
				continue
			}
			if len(s.Tools) == 0 {
				continue // every tool now: wider
			}
			for _, t := range ps.Tools {
				if !slices.Contains(s.Tools, t) {
					return true
				}
			}
		}
	}
	return false
}

// rulesRevoked reports whether an AccessRule that the change in progress compiled has been deleted or narrowed since
// (read from the API server). Only a change that enforces watches: in audit mode every signed-in request is allowed
// and a removal changes records only. A grant (a new or wider rule) is not a revocation and keeps the order and the
// waits of the change.
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
