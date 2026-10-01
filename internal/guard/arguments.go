package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Grant is one access rule's grant of a server's tools, with its argument limits (requirement 8).
type Grant struct {
	Rule        string
	Groups      []string
	AllSignedIn bool
	Tools       []string // nil: every tool
	Arguments   []v1.ArgumentRule
}

// Rules lists the grants for one server.
type Rules interface {
	Grants(ctx context.Context, server string) []Grant
}

// KubeRules reads grants from AccessRule objects (a cached client, so a check costs no API call).
type KubeRules struct {
	Client    client.Reader
	Namespace string
}

func (k KubeRules) Grants(ctx context.Context, server string) []Grant {
	var l v1.AccessRuleList
	if server == "" || k.Client.List(ctx, &l, client.InNamespace(k.Namespace)) != nil {
		return nil
	}
	var out []Grant
	for _, r := range l.Items {
		for _, sa := range r.Spec.Servers {
			if sa.Name == server {
				out = append(out, Grant{Rule: r.Name, Groups: r.Spec.Groups, AllSignedIn: r.Spec.AllSignedIn, Tools: sa.Tools, Arguments: sa.Arguments})
			}
		}
	}
	return out
}

// CheckArguments returns why a call is refused, or "" when it is allowed. A call is allowed when any rule
// that grants this tool to the caller allows its arguments (rules add up, as in the gateway). When no rule
// grants the tool, the gateway's own authorization decides, so this allows it.
func CheckArguments(grants []Grant, groups []string, tool string, args map[string]any) string {
	var applicable []Grant
	for _, g := range grants {
		if (g.AllSignedIn || slices.ContainsFunc(g.Groups, func(x string) bool { return slices.Contains(groups, x) })) &&
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

func firstFailure(rules []v1.ArgumentRule, args map[string]any) string {
	for _, a := range rules {
		v, present := args[a.Name]
		if len(a.OneOf) > 0 {
			if s, ok := v.(string); !present || !ok || !slices.Contains(a.OneOf, s) {
				return fmt.Sprintf("argument %s must be one of %s", a.Name, strings.Join(a.OneOf, ", "))
			}
		}
		if len(a.NotContains) > 0 && present {
			text, ok := v.(string)
			if !ok {
				b, _ := json.Marshal(v)
				text = string(b)
			}
			for _, bad := range a.NotContains {
				if strings.Contains(strings.ToLower(text), strings.ToLower(bad)) {
					return fmt.Sprintf("argument %s must not contain %s", a.Name, bad)
				}
			}
		}
		if a.Max != "" {
			limit, err := strconv.ParseFloat(a.Max, 64)
			n, ok := v.(float64)
			if err != nil || !present || !ok || n > limit {
				return fmt.Sprintf("argument %s must be a number at most %s", a.Name, a.Max)
			}
		}
	}
	return ""
}
