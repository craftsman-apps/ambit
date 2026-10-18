package console

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sort"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sameGrant reports whether two rules grant exactly the same access. Order and repeated entries inside a list
// do not matter. A server with nil tools (all tools) differs from any list of tools.
// Every field of v1.AccessRuleSpec, v1.ServerAccess and v1.ArgumentRule must be compared here; TestSameGrantComparesEveryField
// fails when a field is added to either struct and this function is not updated.
func sameGrant(a, b v1.AccessRuleSpec) bool {
	if a.AllSignedIn != b.AllSignedIn || !sameSet(a.Groups, b.Groups) || !sameSet(a.Models, b.Models) {
		return false
	}
	as, bs := serverSets(a.Servers), serverSets(b.Servers)
	if len(as) != len(bs) {
		return false
	}
	for name, x := range as {
		y, ok := bs[name]
		if !ok || x.allTools != y.allTools || !sameSet(x.tools, y.tools) || !sameArguments(x.args, y.args) {
			return false
		}
	}
	return true
}

type serverSet struct {
	allTools bool
	tools    []string
	args     []v1.ArgumentRule
}

// serverSets groups servers by name; a repeated server name merges like the controller does (tools add up, all-tools wins).
func serverSets(in []v1.ServerAccess) map[string]serverSet {
	out := map[string]serverSet{}
	for _, s := range in {
		x, seen := out[s.Name]
		if !seen {
			x.allTools = s.Tools == nil
		}
		if s.Tools == nil {
			x.allTools = true
		}
		x.tools = append(x.tools, s.Tools...)
		x.args = append(x.args, s.Arguments...)
		out[s.Name] = x
	}
	return out
}

func sameSet(a, b []string) bool {
	return slices.Equal(uniqSorted(a), uniqSorted(b))
}

func uniqSorted(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}

// sameArguments compares argument limits as an unordered collection; each rule's lists are compared as sets.
func sameArguments(a, b []v1.ArgumentRule) bool {
	key := func(r v1.ArgumentRule) string {
		return fmt.Sprintf("%q|%q|%q|%q", r.Name, uniqSorted(r.OneOf), uniqSorted(r.NotContains), r.Max)
	}
	ka, kb := map[string]bool{}, map[string]bool{}
	for _, r := range a {
		ka[key(r)] = true
	}
	for _, r := range b {
		kb[key(r)] = true
	}
	return reflect.DeepEqual(ka, kb)
}

// duplicateOf returns the name of another rule in the namespace that grants exactly what spec grants ("" for none).
// self is the rule being updated, which is not compared with itself. It reads the cluster only, also in GitOps mode:
// duplicates between open pull requests are not detected, and the pull request review is the second check.
func (c *Catalog) duplicateOf(r *http.Request, spec v1.AccessRuleSpec, self string) (string, error) {
	var rs v1.AccessRuleList
	if err := c.Client.List(r.Context(), &rs, client.InNamespace(c.Namespace)); err != nil {
		return "", err
	}
	sort.Slice(rs.Items, func(i, j int) bool { return rs.Items[i].Name < rs.Items[j].Name })
	for _, o := range rs.Items {
		if o.Name != self && sameGrant(spec, o.Spec) {
			return o.Name, nil
		}
	}
	return "", nil
}

// refuseDuplicate answers 409 and returns false when another rule grants exactly spec.
func (c *Catalog) refuseDuplicate(w http.ResponseWriter, r *http.Request, spec v1.AccessRuleSpec, self string) bool {
	name, err := c.duplicateOf(r, spec, self)
	if err != nil {
		serverErr(w, err)
		return false
	}
	if name != "" {
		writeErr(w, http.StatusConflict, "access rule "+name+" already grants exactly this")
		return false
	}
	return true
}
