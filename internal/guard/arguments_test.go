package guard

import (
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

// Requirement 8: rules on tool arguments. "Can it delete the production database?"
func TestCheckArguments(t *testing.T) {
	db := Grant{Rule: "eng-db", Groups: []string{"eng"}, Tools: []string{"query"}, Arguments: []v1.ArgumentRule{
		{Name: "database", OneOf: []string{"analytics", "reporting"}},
		{Name: "sql", NotContains: []string{"drop", "DELETE"}},
	}}
	pay := Grant{Rule: "finance-pay", Groups: []string{"finance"}, Tools: []string{"pay"}, Arguments: []v1.ArgumentRule{{Name: "amount", Max: "1000"}}}
	admin := Grant{Rule: "admins-db", Groups: []string{"dba"}, Tools: nil} // every tool, no limits
	grants := []Grant{db, pay, admin}
	for _, tc := range []struct {
		name   string
		groups []string
		tool   string
		args   map[string]any
		want   string // "" = allowed; else a part of the reason
	}{
		{"allowed database and query", []string{"eng"}, "query", map[string]any{"database": "analytics", "sql": "select 1"}, ""},
		{"database not in the list", []string{"eng"}, "query", map[string]any{"database": "prod", "sql": "select 1"}, "database must be one of analytics, reporting"},
		{"database missing", []string{"eng"}, "query", map[string]any{"sql": "select 1"}, "database must be one of"},
		{"database not a string", []string{"eng"}, "query", map[string]any{"database": 7, "sql": "x"}, "database must be one of"},
		{"forbidden word, other case", []string{"eng"}, "query", map[string]any{"database": "analytics", "sql": "Drop table users"}, "sql must not contain drop"},
		{"forbidden word in a non-string value", []string{"eng"}, "query", map[string]any{"database": "analytics", "sql": []any{"DELETE"}}, "must not contain DELETE"},
		{"amount within the limit", []string{"finance"}, "pay", map[string]any{"amount": 999.5}, ""},
		{"amount over the limit", []string{"finance"}, "pay", map[string]any{"amount": 1000.01}, "amount must be a number at most 1000"},
		{"amount as text", []string{"finance"}, "pay", map[string]any{"amount": "10"}, "amount must be a number"},
		{"amount missing", []string{"finance"}, "pay", map[string]any{}, "amount must be a number"},
		{"another rule grants the tool without limits", []string{"eng", "dba"}, "query", map[string]any{"database": "prod", "sql": "drop table x"}, ""},
		{"no grant applies: the gateway decides", []string{"sales"}, "query", map[string]any{"database": "prod"}, ""},
		{"a different tool of the server is not limited", []string{"eng"}, "other", map[string]any{}, ""},
	} {
		reason := CheckArguments(grants, tc.groups, tc.tool, tc.args)
		if (tc.want == "") != (reason == "") || !strings.Contains(reason, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, reason, tc.want)
		}
	}
	// Everyone signed in, and two limited rules: allowed when either one is satisfied.
	a := Grant{Rule: "a", AllSignedIn: true, Arguments: []v1.ArgumentRule{{Name: "region", OneOf: []string{"eu"}}}}
	b := Grant{Rule: "b", Groups: []string{"us-team"}, Arguments: []v1.ArgumentRule{{Name: "region", OneOf: []string{"us"}}}}
	if r := CheckArguments([]Grant{a, b}, []string{"us-team"}, "x", map[string]any{"region": "us"}); r != "" {
		t.Errorf("rule b should allow: %s", r)
	}
	if r := CheckArguments([]Grant{a, b}, nil, "x", map[string]any{"region": "us"}); !strings.Contains(r, "region must be one of eu") {
		t.Errorf("only rule a applies: %q", r)
	}
}
