package controller

import (
	"slices"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// Every generated line must compile (agentgateway accepts invalid CEL silently and treats the line as false, spec
// fact 12), and a category line is true only with the published hash and the category.
func TestCategoryLineNeedsPublishedHash(t *testing.T) {
	res, _, cfg := compileLLM(append(slices.Clone(catalog), local), grants, installed, llmRouter("auto", "coding"))
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("request", cel.DynType), cel.Variable("llmRequest", cel.DynType), cel.Variable("jwt", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	eval := func(line string, headers map[string]any) bool {
		t.Helper()
		ast, iss := env.Compile(line)
		if iss.Err() != nil {
			t.Fatalf("does not compile: %v\n%s", iss.Err(), line)
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		v, _, err := prg.Eval(map[string]any{"jwt": map[string]any{"groups": []any{"ml"}},
			"request":    map[string]any{"path": "/v1/chat/completions", "headers": headers},
			"llmRequest": map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}})
		return err == nil && v.Value() == true
	}
	ts := targets(t, byName(res.Objects)["router-auto"])
	for _, l := range ts {
		eval(l[0], map[string]any{}) // every line compiles
	}
	for name, h := range map[string]map[string]any{
		"no label":       {},
		"label, no hash": {"x-ambit-route": "coding"},
		"old hash":       {"x-ambit-route": "coding", "x-ambit-config": "0000"},
		"other category": {"x-ambit-route": "maths", "x-ambit-config": cfg.Hash},
	} {
		if eval(ts[0][0], h) {
			t.Errorf("%s: category line must be false", name)
		}
	}
	if !eval(ts[0][0], map[string]any{"x-ambit-route": "coding", "x-ambit-config": cfg.Hash}) {
		t.Error("published hash + category must match for a user allowed the router and the target")
	}
}
