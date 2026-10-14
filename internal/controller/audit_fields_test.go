package controller

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// auditCatalog: two Models with one spec.model (each granted to another group), one for everyone signed in, one
// without a rule; a router whose default (haiku) only eng may use and whose line goes to sonnet, which only ml may
// use; one MCP
// server granted whole to ml and its tool "now" to eng.
func auditCatalog(t *testing.T) (Result, []any) {
	t.Helper()
	models := []v1.Model{model("gpt-a", "openai/gpt"), model("gpt-b", "openai/gpt"), model("gemma", "google/gemma"),
		model("secret", "x/secret"), model("sonnet", "anthropic/sonnet"), model("haiku", "anthropic/haiku")}
	rules := []v1.AccessRule{
		rule("eng-gpt", []string{"eng"}, false, "gpt-a"), rule("ops-gpt", []string{"ops"}, false, "gpt-b"),
		rule("all-gemma", nil, true, "gemma"), rule("ml-sonnet", []string{"ml"}, false, "sonnet"),
		rule("eng-haiku", []string{"eng"}, false, "haiku"), rule("auto-users", []string{"eng", "ml"}, false, "auto"),
		toolRule("ml-time", []string{"ml"}, false, v1.ServerAccess{Name: "time"}),
		toolRule("eng-now", []string{"eng"}, false, v1.ServerAccess{Name: "time", Tools: []string{"now"}}),
	}
	routers := []v1.ModelRouter{router("auto", "Rules", "haiku", v1.RouterRoute{Model: "sonnet", Keywords: []string{"python"}})}
	res := Compile(models, []v1.Server{server("time", "stdio")}, rules, route, "auto")
	CompileRouters(routers, models, &res, route, nil, ClassifierInput{})
	fields, err := AuditFields(res, 50)
	if err != nil {
		t.Fatal(err)
	}
	return res, fields
}

func field(t *testing.T, fields []any, name string) string {
	t.Helper()
	for _, f := range fields {
		if m := f.(map[string]any); m["name"] == name {
			return m["expression"].(string)
		}
	}
	t.Fatalf("no field %s in %v", name, fields)
	return ""
}

var auditEnv = func() *cel.Env {
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("jwt", cel.DynType), cel.Variable("apiKey", cel.DynType),
		cel.Variable("llm", cel.DynType), cel.Variable("metadata", cel.DynType), cel.Variable("mcp", cel.DynType))
	if err != nil {
		panic(err)
	}
	return env
}()

// evalField evaluates one log field like agentgateway: an error leaves the field out ("" here).
func evalField(t *testing.T, expr string, act map[string]any) string {
	t.Helper()
	ast, iss := auditEnv.Compile(expr)
	if iss.Err() != nil {
		t.Fatalf("does not compile: %v\n%s", iss.Err(), expr)
	}
	prg, err := auditEnv.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := prg.Eval(act)
	if err != nil {
		return ""
	}
	return fmt.Sprint(v.Value())
}

// llmCall is an LLM record's activation: the person's groups by JWT, the model asked for (router wire name or
// model) and the model that answered.
func llmCall(groups []any, asked, answered string) map[string]any {
	return map[string]any{"jwt": map[string]any{"groups": groups}, "apiKey": map[string]any{},
		"llm": map[string]any{"requestModel": answered}, "metadata": map[string]any{"agentgateway_user_model": asked}}
}

func TestWouldRefuseModelsAndRouters(t *testing.T) {
	res, fields := auditCatalog(t)
	expr := field(t, fields, "ambit.wouldRefuse")
	wire, _, _ := unstructured.NestedString(byName(res.Objects)["router-auto"].Object, "spec", "match", "model")
	for _, c := range []struct {
		name string
		act  map[string]any
		want string
	}{
		{"group grant", llmCall([]any{"eng"}, "openai/gpt", "openai/gpt"), "allow"},
		{"two Models with one spec.model: the other one allows", llmCall([]any{"ops"}, "openai/gpt", "openai/gpt"), "allow"},
		{"no group granted", llmCall([]any{"sales"}, "openai/gpt", "openai/gpt"), "refuse:model"},
		{"all signed in", llmCall([]any{}, "google/gemma", "google/gemma"), "allow"},
		{"no rule", llmCall([]any{"eng"}, "x/secret", "x/secret"), "refuse:model"},
		{"unknown model", llmCall([]any{"eng"}, "y/unknown", "y/unknown"), "n/a"},
		{"key groups, not the JWT's", map[string]any{"jwt": map[string]any{"groups": []any{"sales"}},
			"apiKey": map[string]any{"keyId": "k1", "groups": []any{"eng"}}, "llm": map[string]any{"requestModel": "openai/gpt"},
			"metadata": map[string]any{"agentgateway_user_model": "openai/gpt"}}, "allow"},
		{"key without the group, JWT with it", map[string]any{"jwt": map[string]any{"groups": []any{"eng"}},
			"apiKey": map[string]any{"keyId": "k1", "groups": []any{"sales"}}, "llm": map[string]any{"requestModel": "openai/gpt"},
			"metadata": map[string]any{"agentgateway_user_model": "openai/gpt"}}, "refuse:model"},
		{"router without router access", llmCall([]any{"sales"}, wire, "openai/gpt"), "refuse:router"},
		{"router, default answered, allowed", llmCall([]any{"eng"}, wire, "anthropic/haiku"), "allow"},
		{"router, line answered, only the default allowed", llmCall([]any{"eng"}, wire, "anthropic/sonnet"), "other-model"},
		{"router, line answered, allowed", llmCall([]any{"ml"}, wire, "anthropic/sonnet"), "allow"},
		{"router, default answered, not allowed", llmCall([]any{"ml"}, wire, "anthropic/haiku"), "refuse:model"},
		{"MCP record", map[string]any{"jwt": map[string]any{"groups": []any{"eng"}}, "apiKey": map[string]any{},
			"mcp": map[string]any{"tool": map[string]any{"target": "time", "name": "now"}}}, ""},
	} {
		if got := evalField(t, expr, c.act); got != c.want && !(c.want == "" && got == "n/a") {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWouldRefuseTool(t *testing.T) {
	_, fields := auditCatalog(t)
	expr := field(t, fields, "ambit.wouldRefuseTool")
	call := func(groups []any, target, tool string) map[string]any {
		return map[string]any{"jwt": map[string]any{"groups": groups}, "apiKey": map[string]any{},
			"mcp": map[string]any{"tool": map[string]any{"target": target, "name": tool}}}
	}
	for _, c := range []struct {
		name string
		act  map[string]any
		want string
	}{
		{"whole-server grant", call([]any{"ml"}, "time", "zone"), "allow"},
		{"per-tool grant", call([]any{"eng"}, "time", "now"), "allow"},
		{"per-tool grant, other tool", call([]any{"eng"}, "time", "zone"), "refuse:tool"},
		{"no grant", call([]any{"sales"}, "time", "now"), "refuse:tool"},
		{"unknown tool of another server", call([]any{"ml"}, "github", "create_issue"), "refuse:tool"},
		{"tools/list (no tool)", map[string]any{"jwt": map[string]any{"groups": []any{"ml"}}, "apiKey": map[string]any{}, "mcp": map[string]any{}}, "n/a"},
		{"LLM record", llmCall([]any{"eng"}, "openai/gpt", "openai/gpt"), ""},
	} {
		if got := evalField(t, expr, c.act); got != c.want && !(c.want == "" && got == "n/a") {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// One generator: the fields hold the very strings the enforce compile writes into the authorization objects.
func TestAuditFieldsReuseTheEnforceExpressions(t *testing.T) {
	res, fields := auditCatalog(t)
	all := field(t, fields, "ambit.wouldRefuse") + field(t, fields, "ambit.wouldRefuseTool")
	n := 0
	for _, o := range res.Objects {
		var e []string
		switch {
		case o.GetKind() == "AgentgatewayModel" && o.GetLabels()[RouterLabel] == "":
			e, _, _ = unstructured.NestedStringSlice(o.Object, "spec", "policies", "authorization", "policy", "matchExpressions")
		case o.GetName() == MCPBackendName+"-access":
			e, _, _ = unstructured.NestedStringSlice(o.Object, "spec", "backend", "mcp", "authorization", "policy", "matchExpressions")
		}
		for _, x := range e {
			n++
			if !strings.Contains(all, "("+x+")") {
				t.Errorf("%s: %q is not in the fields", o.GetName(), x)
			}
		}
	}
	if n != 7 {
		t.Fatalf("checked %d expressions, want 7", n)
	}
	if got := field(t, fields, "ambit.wouldRefuse"); !strings.Contains(got, "("+CEL(res.Access["auto"])+")") {
		t.Errorf("the router's access expression is not in ambit.wouldRefuse")
	}
}

// Above 16000 characters the model branches are split over ambit.wouldRefuse.1, .2, ...; each ends in "n/a", so the
// console takes the first value that is not "n/a". Past the attribute ceiling it is AuditTooLarge.
func TestWouldRefuseChunks(t *testing.T) {
	var models []v1.Model
	var rules []v1.AccessRule
	for i := range 40 {
		name := fmt.Sprintf("m%02d", i)
		models = append(models, model(name, "vendor/"+name))
		var groups []string
		for g := range 40 {
			groups = append(groups, fmt.Sprintf("team-%02d-%02d-with-a-long-name", i, g))
		}
		rules = append(rules, rule("r-"+name, groups, false, name))
	}
	res := Compile(models, nil, rules, route)
	fields, err := AuditFields(res, 50)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []string
	for _, f := range fields {
		m := f.(map[string]any)
		name := m["name"].(string)
		if name == "ambit.wouldRefuse" {
			t.Fatal("one field above the size limit")
		}
		if strings.HasPrefix(name, "ambit.wouldRefuse.") {
			if want := fmt.Sprintf("ambit.wouldRefuse.%d", len(chunks)+1); name != want {
				t.Fatalf("chunk %q, want %q", name, want)
			}
			e := m["expression"].(string)
			if len(e) > 16000 || !strings.HasSuffix(e, `"n/a"`) {
				t.Fatalf("chunk %s: %d characters, ends %q", name, len(e), e[len(e)-10:])
			}
			chunks = append(chunks, e)
		}
	}
	if len(chunks) < 2 {
		t.Fatalf("%d chunks", len(chunks))
	}
	last := llmCall([]any{"team-39-00-with-a-long-name"}, "vendor/m39", "vendor/m39")
	var got []string
	for _, c := range chunks {
		got = append(got, evalField(t, c, last))
	}
	if i := slices.IndexFunc(got, func(v string) bool { return v != "n/a" }); i < 1 || got[i] != "allow" {
		t.Errorf("values per chunk %v: want n/a before the chunk that holds m39, then allow", got)
	}
	_, err = AuditFields(res, len(chunks)-1)
	var tooLarge *AuditTooLargeError
	if !errors.As(err, &tooLarge) || !strings.Contains(err.Error(), fmt.Sprintf("%d attributes", len(chunks))) {
		t.Errorf("err %v, want AuditTooLarge naming %d attributes", err, len(chunks))
	}
}
