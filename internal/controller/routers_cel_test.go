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
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("request", cel.DynType), cel.Variable("llmRequest", cel.DynType), cel.Variable("jwt", cel.DynType), cel.Variable("apiKey", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	jwtOnly := map[string]any{"jwt": map[string]any{"groups": []any{"ml"}}, "apiKey": map[string]any{}}
	byKey := map[string]any{"jwt": map[string]any{}, "apiKey": map[string]any{"keyId": "k1", "email": "alice@acme.com",
		"groups": []any{"ml"}, "until": "2099-01-01T00:00:00Z"}}
	evalAs := func(act map[string]any, line string, headers map[string]any) bool {
		t.Helper()
		ast, iss := env.Compile(line)
		if iss.Err() != nil {
			t.Fatalf("does not compile: %v\n%s", iss.Err(), line)
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		in := map[string]any{"request": map[string]any{"path": "/v1/chat/completions", "headers": headers},
			"llmRequest": map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}}
		for k, v := range act {
			in[k] = v
		}
		v, _, err := prg.Eval(in)
		return err == nil && v.Value() == true
	}
	eval := func(line string, headers map[string]any) bool { t.Helper(); return evalAs(jwtOnly, line, headers) }
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
	// A request authenticated by a key of the same person gets the same decision as their JWT.
	ok := map[string]any{"x-ambit-route": "coding", "x-ambit-config": cfg.Hash}
	if !evalAs(byKey, ts[0][0], ok) || evalAs(byKey, ts[0][0], map[string]any{"x-ambit-route": "coding"}) {
		t.Error("a key must decide like the owner's JWT")
	}
}
