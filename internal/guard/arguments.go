package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
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
		if (g.AllSignedIn || slices.ContainsFunc(g.Groups, func(x string) bool { return slices.Contains(groups, x) }) ||
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

// firstFailure checks one grant's limits. Arguments are decoded with json.Number, so a maximum is compared
// exactly, and notContains sees every string (and key) inside a nested value as the server will receive it.
func firstFailure(rules []v1.ArgumentRule, args map[string]any) string {
	for _, a := range rules {
		v, present := args[a.Name]
		if len(a.OneOf) > 0 {
			if s, ok := v.(string); !present || !ok || !slices.Contains(a.OneOf, s) {
				return fmt.Sprintf("argument %s must be one of %s", a.Name, strings.Join(a.OneOf, ", "))
			}
		}
		if len(a.NotContains) > 0 && present {
			for _, text := range texts(v, nil) {
				for _, bad := range a.NotContains {
					if strings.Contains(strings.ToLower(text), strings.ToLower(bad)) {
						return fmt.Sprintf("argument %s must not contain %s", a.Name, bad)
					}
				}
			}
		}
		if a.Max != "" {
			limit, okLimit := new(big.Rat).SetString(a.Max)
			n, isNumber := v.(json.Number)
			value, okValue := new(big.Rat).SetString(string(n))
			if !okLimit || !present || !isNumber || !okValue || value.Cmp(limit) > 0 {
				return fmt.Sprintf("argument %s must be a number at most %s", a.Name, a.Max)
			}
		}
	}
	return ""
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
