package identity

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

type doc = map[string]any

func render(t *testing.T, args ...string) []doc {
	t.Helper()
	a := append([]string{"template", "ambit", "../../charts/ambit", "-n", "ambit-system", "-f", "../../deploy/quickstart.yaml"}, args...)
	out, err := exec.Command("helm", a...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var docs []doc
	for _, part := range strings.Split("\n"+string(out), "\n---\n") {
		var d doc
		if err := yaml.Unmarshal([]byte(part), &d); err != nil {
			t.Fatalf("decode: %v\n%s", err, part)
		}
		if len(d) > 0 {
			docs = append(docs, d)
		}
	}
	return docs
}

// get walks maps by key.
func get(t *testing.T, v any, path ...string) any {
	t.Helper()
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("no map at %q (path %v)", k, path)
		}
		v = m[k]
	}
	return v
}

func find(t *testing.T, docs []doc, kind, name string) doc {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == kind && get(t, d, "metadata", "name") == name {
			return d
		}
	}
	t.Fatalf("%s %s not rendered", kind, name)
	return nil
}

func contains(list any, want string) bool {
	l, _ := list.([]any)
	for _, x := range l {
		if x == want {
			return true
		}
	}
	return false
}

func attr(t *testing.T, docs []doc, name string) string {
	t.Helper()
	// The chart renders the access log's static part; ambit-controller writes the policy from it.
	var spec doc
	if err := yaml.Unmarshal([]byte(get(t, find(t, docs, "ConfigMap", "ambit-access-log-base"), "data", "spec.yaml").(string)), &spec); err != nil {
		t.Fatal(err)
	}
	for _, a := range get(t, spec, "frontend", "accessLog", "attributes", "add").([]any) {
		if get(t, a, "name") == name {
			return get(t, a, "expression").(string)
		}
	}
	t.Fatalf("attribute %s missing", name)
	return ""
}

// The chart's policies must decode to exactly the fragments the controller uses.
func TestChartUsesTheSameFragments(t *testing.T) {
	for _, re := range []bool{true, false} {
		var docs []doc
		if re {
			docs = render(t)
		} else {
			docs = render(t, "--set", "llm.jwt.requireEmail=false", "--set", "mcp.jwt.requireEmail=false")
		}
		for _, n := range []string{"ambit-llm-auth", "ambit-mcp-auth"} {
			p := find(t, docs, "AgentgatewayPolicy", n)
			if !contains(get(t, p, "spec", "traffic", "authorization", "policy", "matchExpressions"), Gate(re)) {
				t.Errorf("%s (requireEmail=%v): matchExpressions lack Gate: %v", n, re, get(t, p, "spec", "traffic", "authorization", "policy", "matchExpressions"))
			}
		}
	}
	docs := render(t)
	for _, n := range []string{"jwt.email", "jwt.groups", "jwt.sub", "ambit.key_id", "ambit.identity"} {
		e := attr(t, docs, n)
		if n == "jwt.email" || n == "jwt.sub" || n == "ambit.identity" {
			if !strings.Contains(e, ValidKey) || !strings.Contains(e, PresentedKey) {
				t.Errorf("attribute %s lacks ValidKey or PresentedKey: %s", n, e)
			}
		}
	}
	for _, verified := range []bool{true, false} {
		d := render(t, "--set", "protection.enabled=true", "--set", "accessRules.usersRequireVerifiedEmail="+strconv.FormatBool(verified))
		g := find(t, d, "AgentgatewayPolicy", "ambit-mcp-guard")
		var groups, email any
		for _, p := range get(t, g, "spec", "backend", "mcp", "guardrails", "processors").([]any) {
			groups, email = get(t, p, "remote", "metadata", "groups"), get(t, p, "remote", "metadata", "email")
		}
		if groups != PrincipalGroups {
			t.Errorf("guard groups = %v, want %q", groups, PrincipalGroups)
		}
		if email != PrincipalEmail(verified) {
			t.Errorf("verified=%v: guard email = %v, want %q", verified, email, PrincipalEmail(verified))
		}
		// The router's ambit.email comes from the controller, which gets the same value as a flag.
		if !contains(get(t, find(t, d, "Deployment", "ambit-controller"), "spec", "template", "spec", "containers").([]any)[0].(doc)["command"],
			"--users-require-verified-email="+strconv.FormatBool(verified)) {
			t.Errorf("verified=%v: controller flag missing", verified)
		}
	}
}

// A model route and the MCP backend belong to their own route only.
func TestRoutesAreExclusive(t *testing.T) {
	for _, d := range render(t) {
		if d["kind"] != "HTTPRoute" {
			continue
		}
		name := get(t, d, "metadata", "name")
		for _, r := range get(t, d, "spec", "rules").([]any) {
			refs, _ := get(t, r, "backendRefs").([]any)
			for _, b := range refs {
				bm := b.(map[string]any)
				if bm["kind"] == "AgentgatewayModel" && name != "ambit-llm" {
					t.Errorf("route %v references AgentgatewayModel", name)
				}
				if bm["name"] == "ambit-mcp" && name != "ambit-mcp" {
					t.Errorf("route %v references ambit-mcp", name)
				}
			}
		}
	}
}

// With TLS on, plain HTTP on every enabled Ambit host redirects to HTTPS.
func TestHTTPSRedirectListsEnabledHosts(t *testing.T) {
	hosts := func(args ...string) []any {
		d := find(t, render(t, append([]string{"--set", "gateway.tls.enabled=true", "--set", "gateway.tls.certificateSecret=tls"}, args...)...), "HTTPRoute", "ambit-https-redirect")
		return get(t, d, "spec", "hostnames").([]any)
	}
	if h := hosts("--set", "mcp.enabled=true", "--set", "mcp.host=mcp.example.com"); !contains(h, "mcp.example.com") {
		t.Errorf("mcp on: redirect hosts %v lack the MCP host", h)
	}
	if h := hosts("--set", "mcp.enabled=false", "--set", "mcp.host=mcp.example.com"); contains(h, "mcp.example.com") {
		t.Errorf("mcp off: redirect hosts %v list the MCP host", h)
	}
}

// The console may create Models only in Direct mode; in GitOps mode a Model changes only through a pull request.
func TestConsoleCreatesModelsOnlyInDirectMode(t *testing.T) {
	creates := func(docs []doc) bool {
		for _, r := range get(t, find(t, docs, "Role", "ambit-console"), "rules").([]any) {
			if contains(get(t, r, "resources"), "models") && contains(get(t, r, "verbs"), "create") {
				return true
			}
		}
		return false
	}
	if !creates(render(t, "--set", "gitops.enabled=false")) {
		t.Error("Direct mode: the console cannot create Models")
	}
	gitops := render(t, "--set", "gitops.enabled=true", "--set", "gitops.repository=acme/cfg", "--set", "gitops.github.appID=1")
	if creates(gitops) {
		t.Error("GitOps mode: the console can create Models")
	}
}
