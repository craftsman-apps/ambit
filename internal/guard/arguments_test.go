package guard

import (
	"encoding/json"
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
		{"database not a string", []string{"eng"}, "query", map[string]any{"database": json.Number("7"), "sql": "x"}, "database must be one of"},
		{"forbidden word, other case", []string{"eng"}, "query", map[string]any{"database": "analytics", "sql": "Drop table users"}, "sql must not contain drop"},
		{"forbidden word in a non-string value", []string{"eng"}, "query", map[string]any{"database": "analytics", "sql": []any{"DELETE"}}, "must not contain DELETE"},
		{"amount within the limit", []string{"finance"}, "pay", map[string]any{"amount": json.Number("999.5")}, ""},
		{"amount over the limit", []string{"finance"}, "pay", map[string]any{"amount": json.Number("1000.01")}, "amount must be a number at most 1000"},
		{"forbidden text with escaped characters in a nested value", []string{"eng"}, "query", map[string]any{"database": "analytics", "sql": map[string]any{"q": []any{"x <DROP> y"}}}, "sql must not contain drop"},
		{"forbidden text as a nested key", []string{"eng"}, "query", map[string]any{"database": "analytics", "sql": map[string]any{"drop": true}}, "sql must not contain drop"},
		{"amount exactly at the limit", []string{"finance"}, "pay", map[string]any{"amount": json.Number("1000")}, ""},
		{"amount over the limit by less than float precision", []string{"finance"}, "pay", map[string]any{"amount": json.Number("1000.0000000000000001")}, "amount must be a number at most 1000"},
		{"amount in exponent form over the limit", []string{"finance"}, "pay", map[string]any{"amount": json.Number("1e4")}, "amount must be a number at most 1000"},
		{"amount as text", []string{"finance"}, "pay", map[string]any{"amount": "10"}, "amount must be a number"},
		{"amount missing", []string{"finance"}, "pay", map[string]any{}, "amount must be a number"},
		{"another rule grants the tool without limits", []string{"eng", "dba"}, "query", map[string]any{"database": "prod", "sql": "drop table x"}, ""},
		{"no grant applies: the gateway decides", []string{"sales"}, "query", map[string]any{"database": "prod"}, ""},
		{"a different tool of the server is not limited", []string{"eng"}, "other", map[string]any{}, ""},
	} {
		reason := CheckArguments(grants, tc.groups, "", tc.tool, tc.args)
		if (tc.want == "") != (reason == "") || !strings.Contains(reason, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, reason, tc.want)
		}
	}
	// Everyone signed in, and two limited rules: allowed when either one is satisfied.
	a := Grant{Rule: "a", AllSignedIn: true, Arguments: []v1.ArgumentRule{{Name: "region", OneOf: []string{"eu"}}}}
	b := Grant{Rule: "b", Groups: []string{"us-team"}, Arguments: []v1.ArgumentRule{{Name: "region", OneOf: []string{"us"}}}}
	if r := CheckArguments([]Grant{a, b}, []string{"us-team"}, "", "x", map[string]any{"region": "us"}); r != "" {
		t.Errorf("rule b should allow: %s", r)
	}
	if r := CheckArguments([]Grant{a, b}, nil, "", "x", map[string]any{"region": "us"}); !strings.Contains(r, "region must be one of eu") {
		t.Errorf("only rule a applies: %q", r)
	}
}

// A grant to named people applies to the caller with that exact canonical email, and never to a caller without one.
func TestCheckArgumentsUsers(t *testing.T) {
	g := Grant{Rule: "pilot", Users: []string{"user01@acme.com"}, Arguments: []v1.ArgumentRule{{Name: "region", OneOf: []string{"eu"}}}}
	for _, tc := range []struct {
		name, email, want string
	}{
		{"listed user, allowed value", "user01@acme.com", ""},
		{"listed user, refused value", "user01@acme.com", "region must be one of eu"},
		{"another user: not this grant, the gateway decides", "user02@acme.com", ""},
		{"no email: not this grant", "", ""},
	} {
		region := "us"
		if tc.name == "listed user, allowed value" {
			region = "eu"
		}
		r := CheckArguments([]Grant{g}, nil, tc.email, "x", map[string]any{"region": region})
		if (tc.want == "") != (r == "") || !strings.Contains(r, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, r, tc.want)
		}
	}
}

// allSignedIn grants every person, not a service account (SA-8 B): its argument limits never apply to one.
func TestCheckArgumentsAllSignedInSkipsServiceAccounts(t *testing.T) {
	loose := Grant{Rule: "everyone", AllSignedIn: true}
	strict := Grant{Rule: "ci", Groups: []string{"eng-ci"}, Arguments: []v1.ArgumentRule{{Name: "region", OneOf: []string{"eu"}}}}
	args := map[string]any{"region": "us"}
	if r := CheckArguments([]Grant{loose, strict}, []string{"eng-ci"}, "user01@acme.com", "x", args); r != "" {
		t.Errorf("a person gets the open rule: %q", r)
	}
	if r := CheckArguments([]Grant{loose, strict}, []string{"eng-ci"}, "ci@svc.ambit.internal", "x", args); !strings.Contains(r, "region must be one of eu") {
		t.Errorf("a service account must get only its group's rule: %q", r)
	}
}

// Issue 302: min is a lower bound next to max. Rules without min keep their old meaning: max alone allows 0 and
// negative numbers.
func TestArgumentMin(t *testing.T) {
	rows := Grant{Rule: "export", Groups: []string{"eng"}, Arguments: []v1.ArgumentRule{{Name: "limit", Min: "1", Max: "100"}}}
	refund := Grant{Rule: "refund", Groups: []string{"support"}, Arguments: []v1.ArgumentRule{{Name: "amount", Min: "0.01"}}}
	old := Grant{Rule: "old", Groups: []string{"ops"}, Arguments: []v1.ArgumentRule{{Name: "limit", Max: "100"}}}
	for _, tc := range []struct {
		name  string
		g     Grant
		value any // nil: argument missing
		want  string
	}{
		{"lowest allowed value", rows, json.Number("1"), ""},
		{"highest allowed value", rows, json.Number("100"), ""},
		{"inside the range", rows, json.Number("25"), ""},
		{"zero, below min", rows, json.Number("0"), "limit must be a number at least 1 and at most 100"},
		{"negative", rows, json.Number("-1"), "at least 1 and at most 100"},
		{"just below min, finer than float precision", rows, json.Number("0.99999999999999999999"), "at least 1"},
		{"huge negative exponent form", rows, json.Number("-1e400"), "at least 1"},
		{"above max", rows, json.Number("101"), "at least 1 and at most 100"},
		{"missing", rows, nil, "limit must be a number"},
		{"as text", rows, "5", "limit must be a number"},
		{"min alone: at min", refund, json.Number("0.01"), ""},
		{"min alone: large value", refund, json.Number("1e9"), ""},
		{"min alone: below", refund, json.Number("0"), "amount must be a number at least 0.01"},
		{"no min (older rule): zero passes", old, json.Number("0"), ""},
		{"no min (older rule): negative passes", old, json.Number("-1"), ""},
		{"no min (older rule): above max", old, json.Number("101"), "limit must be a number at most 100"},
	} {
		args := map[string]any{}
		name := tc.g.Arguments[0].Name
		if tc.value != nil {
			args[name] = tc.value
		}
		r := CheckArguments([]Grant{tc.g}, tc.g.Groups, "", "x", args)
		if (tc.want == "") != (r == "") || !strings.Contains(r, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, r, tc.want)
		}
	}
	// A min limit also holds for every letter case of the name (issue 334).
	r := CheckArguments([]Grant{rows}, []string{"eng"}, "", "x", map[string]any{"limit": json.Number("5"), "LIMIT": json.Number("-1")})
	if !strings.Contains(r, "at least 1") {
		t.Errorf("other letter case passed min: %q", r)
	}
}
