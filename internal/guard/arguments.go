package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/identity"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Grant is one access rule's grant of a server's tools, with its argument limits (requirement 8).
type Grant struct {
	Rule        string
	Groups      []string
	Users       []string // canonical emails
	AllSignedIn bool
	Tools       []string // nil: every tool
	Arguments   []v1.ArgumentRule
}

// Rules lists the grants for one server. An error means the rules are unknown, and the call is refused.
type Rules interface {
	Grants(ctx context.Context, server string) ([]Grant, error)
}

// KubeRules reads grants from AccessRule objects (a cached client, so a check costs no API call).
type KubeRules struct {
	Client    client.Reader
	Namespace string
}

func (k KubeRules) Grants(ctx context.Context, server string) ([]Grant, error) {
	var l v1.AccessRuleList
	if err := k.Client.List(ctx, &l, client.InNamespace(k.Namespace)); err != nil {
		return nil, err
	}
	var out []Grant
	for _, r := range l.Items {
		for _, sa := range r.Spec.Servers {
			if sa.Name == server {
				out = append(out, Grant{Rule: r.Name, Groups: r.Spec.Groups, Users: r.Spec.Users, AllSignedIn: r.Spec.AllSignedIn, Tools: sa.Tools, Arguments: sa.Arguments})
			}
		}
	}
	return out, nil
}

// Limited reports whether any grant limits arguments; only then do the caller's groups matter.
func Limited(grants []Grant) bool {
	return slices.ContainsFunc(grants, func(g Grant) bool { return len(g.Arguments) > 0 })
}

// CheckArguments returns why a call is refused, or "" when it is allowed. A call is allowed when any rule
// that grants this tool to the caller allows its arguments (rules add up, as in the gateway). When no rule
// grants the tool, the gateway's own authorization decides, so this allows it. email is the caller's canonical
// email, or "" without one (it then matches no user).
func CheckArguments(grants []Grant, groups []string, email string, tool string, args map[string]any) string {
	var applicable []Grant
	for _, g := range grants {
		if ((g.AllSignedIn && !identity.IsService(email)) || slices.ContainsFunc(g.Groups, func(x string) bool { return slices.Contains(groups, x) }) ||
			(email != "" && slices.Contains(g.Users, email))) &&
			(g.Tools == nil || slices.Contains(g.Tools, tool)) {
			applicable = append(applicable, g)
		}
	}
	reason := ""
	for _, g := range applicable {
		r := firstFailure(g.Arguments, args)
		if r == "" {
			return ""
		}
		if reason == "" {
			reason = "rule " + g.Rule + ": " + r
		}
	}
	return reason
}

// firstFailure checks one grant's limits. Arguments are decoded with json.Number, so min and max are compared
// exactly, and notContains sees every string (and key) inside a nested value as the server will receive it.
// A limit holds for every key that equals its name without regard to case (strings.EqualFold, as Go's
// encoding/json matches field names: "Query", "QUERY" and "ſql" reach a Go server as query and sql, issue 334).
func firstFailure(rules []v1.ArgumentRule, args map[string]any) string {
	for _, a := range rules {
		var values []any
		for k, v := range args {
			if strings.EqualFold(k, a.Name) {
				values = append(values, v)
			}
		}
		if len(a.OneOf) > 0 {
			bad := len(values) == 0
			for _, v := range values {
				s, ok := v.(string)
				bad = bad || !ok || !slices.Contains(a.OneOf, s)
			}
			if bad {
				return fmt.Sprintf("argument %s must be one of %s", a.Name, strings.Join(a.OneOf, ", "))
			}
		}
		for _, v := range values {
			for _, text := range texts(v, nil) {
				// Both with and without comments: the server receives the text inside a comment too.
				withComments, withoutComments := spaced(text), folded(text)
				for _, bad := range a.NotContains {
					if b := folded(bad); strings.Contains(withComments, b) || strings.Contains(withoutComments, b) {
						return fmt.Sprintf("argument %s must not contain %s", a.Name, bad)
					}
				}
			}
		}
		if a.Max != "" || a.Min != "" {
			bad := len(values) == 0
			for _, v := range values {
				n, isNumber := v.(json.Number)
				value, okValue := new(big.Rat).SetString(string(n))
				bad = bad || !isNumber || !okValue || outside(value, a.Min, a.Max)
			}
			if bad {
				return fmt.Sprintf("argument %s must be a number%s", a.Name, bounds(a.Min, a.Max))
			}
		}
	}
	return ""
}

// outside reports whether value is below lo (min) or above hi (max); an empty bound is no bound, and a bound that is not a
// number (the CRD pattern prevents it) refuses every value.
func outside(value *big.Rat, lo, hi string) bool {
	for _, b := range []struct {
		limit string
		sign  int
	}{{lo, -1}, {hi, 1}} {
		if b.limit == "" {
			continue
		}
		limit, ok := new(big.Rat).SetString(b.limit)
		if !ok || value.Cmp(limit) == b.sign {
			return true
		}
	}
	return false
}

func bounds(lo, hi string) string {
	switch {
	case lo != "" && hi != "":
		return " at least " + lo + " and at most " + hi
	case lo != "":
		return " at least " + lo
	}
	return " at most " + hi
}

var sqlComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// folded is text for notContains: lower case, SQL comments (/* ... */) as a space, and every run of white space as
// one space, so "DROP  TABLE", "DROP\tTABLE" and "DROP/**/TABLE" all contain "drop table" (issue 301).
func folded(s string) string {
	return spaced(sqlComment.ReplaceAllString(s, " "))
}

// spaced is s in lower case with every run of white space as one space.
func spaced(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// texts collects every string in a decoded JSON value: strings, object keys, and numbers as written.
func texts(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case json.Number:
		out = append(out, string(x))
	case []any:
		for _, e := range x {
			out = texts(e, out)
		}
	case map[string]any:
		for k, e := range x {
			out = texts(e, append(out, k))
		}
	}
	return out
}
