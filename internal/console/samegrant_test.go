package console

import (
	"net/http"
	"reflect"
	"slices"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSameGrant(t *testing.T) {
	srv := func(name string, tools []string, args ...v1.ArgumentRule) v1.ServerAccess {
		return v1.ServerAccess{Name: name, Tools: tools, Arguments: args}
	}
	base := v1.AccessRuleSpec{Groups: []string{"eng", "ops"}, Models: []string{"a", "b"},
		Servers: []v1.ServerAccess{srv("time", []string{"x", "y"}, v1.ArgumentRule{Name: "n", OneOf: []string{"1", "2"}})}}
	cases := []struct {
		name string
		b    v1.AccessRuleSpec
		want bool
	}{
		{"identical", base, true},
		{"order and duplicates", v1.AccessRuleSpec{Groups: []string{"ops", "eng", "eng"}, Models: []string{"b", "a", "a"},
			Servers: []v1.ServerAccess{srv("time", []string{"y", "x", "x"}, v1.ArgumentRule{Name: "n", OneOf: []string{"2", "1"}})}}, true},
		{"different group", v1.AccessRuleSpec{Groups: []string{"eng"}, Models: base.Models, Servers: base.Servers}, false},
		{"allSignedIn", v1.AccessRuleSpec{AllSignedIn: true, Models: base.Models, Servers: base.Servers}, false},
		{"different model", v1.AccessRuleSpec{Groups: base.Groups, Models: []string{"a"}, Servers: base.Servers}, false},
		{"no servers", v1.AccessRuleSpec{Groups: base.Groups, Models: base.Models}, false},
		{"different server", v1.AccessRuleSpec{Groups: base.Groups, Models: base.Models,
			Servers: []v1.ServerAccess{srv("other", []string{"x", "y"}, base.Servers[0].Arguments...)}}, false},
		{"nil tools differ from a list", v1.AccessRuleSpec{Groups: base.Groups, Models: base.Models,
			Servers: []v1.ServerAccess{srv("time", nil, base.Servers[0].Arguments...)}}, false},
		{"different tools", v1.AccessRuleSpec{Groups: base.Groups, Models: base.Models,
			Servers: []v1.ServerAccess{srv("time", []string{"x"}, base.Servers[0].Arguments...)}}, false},
		{"different oneOf", v1.AccessRuleSpec{Groups: base.Groups, Models: base.Models,
			Servers: []v1.ServerAccess{srv("time", []string{"x", "y"}, v1.ArgumentRule{Name: "n", OneOf: []string{"1"}})}}, false},
		{"different max", v1.AccessRuleSpec{Groups: base.Groups, Models: base.Models,
			Servers: []v1.ServerAccess{srv("time", []string{"x", "y"}, v1.ArgumentRule{Name: "n", OneOf: []string{"1", "2"}, Max: "5"})}}, false},
		{"different notContains", v1.AccessRuleSpec{Groups: base.Groups, Models: base.Models,
			Servers: []v1.ServerAccess{srv("time", []string{"x", "y"}, v1.ArgumentRule{Name: "n", OneOf: []string{"1", "2"}, NotContains: []string{"z"}})}}, false},
		{"no arguments", v1.AccessRuleSpec{Groups: base.Groups, Models: base.Models, Servers: []v1.ServerAccess{srv("time", []string{"x", "y"})}}, false},
	}
	for _, tc := range cases {
		if got := sameGrant(base, tc.b); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		if got := sameGrant(tc.b, base); got != tc.want {
			t.Errorf("%s (reversed): got %v, want %v", tc.name, got, tc.want)
		}
	}
	if !sameGrant(v1.AccessRuleSpec{}, v1.AccessRuleSpec{Groups: []string{}}) {
		t.Error("nil and empty lists should match")
	}
	// Users compare as a set too.
	people := v1.AccessRuleSpec{Users: []string{"alice@acme.com", "bob@acme.com"}, Models: []string{"a"}}
	for _, tc := range []struct {
		name  string
		users []string
		want  bool
	}{
		{"same users, other order, repeated", []string{"bob@acme.com", "alice@acme.com", "bob@acme.com"}, true},
		{"one user fewer", []string{"alice@acme.com"}, false},
		{"another user", []string{"alice@acme.com", "carol@acme.com"}, false},
		{"no users", nil, false},
	} {
		b := v1.AccessRuleSpec{Users: tc.users, Models: []string{"a"}}
		if got := sameGrant(people, b); got != tc.want || sameGrant(b, people) != tc.want {
			t.Errorf("users %s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if sameGrant(v1.AccessRuleSpec{Groups: []string{"eng"}, Models: []string{"a"}}, v1.AccessRuleSpec{Groups: []string{"eng"}, Users: []string{"alice@acme.com"}, Models: []string{"a"}}) {
		t.Error("adding a user changes the grant")
	}
}

// sameGrant compares each field by hand. A field added to these structs fails here until sameGrant handles it.
func TestSameGrantComparesEveryField(t *testing.T) {
	for typ, want := range map[reflect.Type][]string{
		reflect.TypeFor[v1.AccessRuleSpec](): {"Groups", "Users", "AllSignedIn", "Models", "Servers"},
		reflect.TypeFor[v1.ServerAccess]():   {"Name", "Tools", "Arguments"},
		reflect.TypeFor[v1.ArgumentRule]():   {"Name", "OneOf", "NotContains", "Max"},
	} {
		var got []string
		for i := range typ.NumField() {
			got = append(got, typ.Field(i).Name)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s fields are %v, sameGrant knows %v: compare the new field in sameGrant, then update this list", typ.Name(), got, want)
		}
	}
}

func TestCreateAndUpdateRefuseDuplicateRules(t *testing.T) {
	h, c, auth := catalogFixture(t)
	admin := as(auth, RoleAdmin)
	count := func() int {
		var l v1.AccessRuleList
		c.List(t.Context(), &l, client.InNamespace(testNS))
		return len(l.Items)
	}
	// everyone-gemma grants AllSignedIn + gemma-4-31b, so the same thing under a new name is refused.
	r := do(t, h, "POST", "/api/access-rules", admin, RuleInput{Name: "copy", AllSignedIn: true, Models: []string{"gemma-4-31b"}})
	if r.Code != http.StatusConflict || !contains(r.Body.String(), "access rule everyone-gemma already grants exactly this") || count() != 2 {
		t.Fatalf("create duplicate: %d %s (rules: %d)", r.Code, r.Body, count())
	}
	// A Git-managed rule counts too.
	if r := do(t, h, "POST", "/api/access-rules", admin, RuleInput{Name: "copy", Groups: []string{"ml-research"}, Models: []string{"nemotron-3-super"}}); r.Code != http.StatusConflict {
		t.Fatalf("duplicate of git rule: %d", r.Code)
	}
	if r := do(t, h, "POST", "/api/access-rules", admin, RuleInput{Name: "eng", Groups: []string{"eng"}, Models: []string{"gemma-4-31b"}}); r.Code != http.StatusCreated {
		t.Fatalf("distinct rule: %d %s", r.Code, r.Body)
	}
	// Update: saving a rule unchanged is allowed; changing it to match another rule is refused and writes nothing.
	if r := do(t, h, "PUT", "/api/access-rules/eng", admin, RuleInput{Groups: []string{"eng"}, Models: []string{"gemma-4-31b"}}); r.Code != 200 {
		t.Fatalf("update unchanged: %d %s", r.Code, r.Body)
	}
	r = do(t, h, "PUT", "/api/access-rules/eng", admin, RuleInput{AllSignedIn: true, Models: []string{"gemma-4-31b"}})
	if r.Code != http.StatusConflict || !contains(r.Body.String(), "everyone-gemma") {
		t.Fatalf("update to duplicate: %d %s", r.Code, r.Body)
	}
	var got v1.AccessRule
	c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "eng"}, &got)
	if got.Spec.AllSignedIn || got.Spec.Groups[0] != "eng" {
		t.Fatalf("refused update was written: %+v", got.Spec)
	}
}

func TestGitOpsRefusesDuplicateBeforeOpeningAPullRequest(t *testing.T) {
	h, _, auth, repo := gitopsFixture(t)
	// eng-time (in the cluster) grants group eng the tool get_current_time on server time.
	r := do(t, h, "POST", "/api/access-rules", as(auth, RoleAdmin), RuleInput{Name: "copy", Groups: []string{"eng"},
		Servers: []v1.ServerAccess{{Name: "time", Tools: []string{"get_current_time"}}}})
	if r.Code != http.StatusConflict || !contains(r.Body.String(), "eng-time") || len(repo.files) != 0 {
		t.Fatalf("%d %s, files proposed: %d", r.Code, r.Body, len(repo.files))
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool { return indexOf(s, sub) >= 0 })()
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
