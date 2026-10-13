package identity

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
)

// A Budget refusal (429) from agentgateway v1.6.0 has an empty body and x-ratelimit-* headers in the ratelimit
// service's units ($0.00001 for dollars). The gateway's response transformation gives it an error body in the API's
// format and removes those headers; other responses keep their body and content type.
func TestBudgetRefusalHasAnErrorBody(t *testing.T) {
	if len(budgetResponse(t, render(t))) != 0 {
		t.Fatal("without limits.enabled the gateway must not rewrite responses")
	}
	r := budgetResponse(t, render(t, "--set", "limits.enabled=true"))
	if r == nil {
		t.Fatal("with limits.enabled the gateway has no response transformation for Budget refusals")
	}
	for _, h := range []string{"x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-reset"} {
		if !contains(r["remove"], h) {
			t.Errorf("remove lacks %s: %v", h, r["remove"])
		}
	}
	meta := get(t, r, "metadata").(map[string]any)
	if len(meta) != 1 {
		t.Fatalf("metadata %v", meta)
	}
	flagExpr := meta["ambitBudgetRefusal"].(string)
	var ctExpr string
	for _, h := range get(t, r, "set").([]any) {
		if get(t, h, "name") == "content-type" {
			ctExpr = get(t, h, "value").(string)
		}
	}
	if ctExpr == "" {
		t.Fatalf("set has no content-type: %v", r["set"])
	}
	bodyExpr := get(t, r, "body").(string)

	env, err := cel.NewEnv(ext.Strings(), cel.Variable("request", cel.DynType), cel.Variable("response", cel.DynType), cel.Variable("metadata", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	eval := func(expr string, in map[string]any) any {
		t.Helper()
		// Parse only: agentgateway's CEL does not type-check, so a ternary may mix a string and null.
		ast, iss := env.Parse(expr)
		if iss.Err() != nil {
			t.Fatalf("does not parse: %v\n%s", iss.Err(), expr)
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		v, _, err := prg.Eval(in)
		if err != nil {
			// agentgateway turns a failed body expression into a 500: every expression must evaluate.
			t.Fatalf("evaluation failed: %v\n%s\ninput %v", err, expr, in)
		}
		if v.Type() == types.NullType {
			return nil
		}
		n, err := v.ConvertToNative(reflect.TypeOf(map[string]any{}))
		if err == nil {
			return n
		}
		return v.Value()
	}
	refusal := map[string]any{"x-ratelimit-limit": "100", "x-ratelimit-remaining": "0", "x-ratelimit-reset": "56493", "retry-after": "56493"}
	cases := []struct {
		name    string
		code    int64
		headers map[string]any
		want    bool
	}{
		{"budget refusal", 429, refusal, true},
		{"provider 429 (has a body and its content type)", 429, merge(refusal, map[string]any{"content-type": "application/json"}), false},
		{"provider 429 without our headers", 429, map[string]any{"retry-after": "20"}, false},
		{"answer", 200, map[string]any{"content-type": "application/json", "x-ratelimit-remaining": "0"}, false},
		{"MCP stream", 200, map[string]any{"content-type": "text/event-stream"}, false},
	}
	for _, c := range cases {
		// Only the LLM endpoints get an error body; never /v1/models or any other path (the MCP host included).
		llm := map[string]bool{"/v1/chat/completions": true, "/v1/completions": true, "/v1/responses": true, "/v1/embeddings": true, "/v1/messages": true}
		for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/embeddings", "/v1/messages", "/v1/models", "/v2/unknown", "/mcp"} {
			want := c.want && llm[path]
			in := map[string]any{"request": map[string]any{"path": path}, "response": map[string]any{"code": c.code, "headers": c.headers}}
			flag := eval(flagExpr, in)
			if flag != want {
				t.Fatalf("%s %s: flag %v, want %v", c.name, path, flag, want)
			}
			in["metadata"] = map[string]any{"ambitBudgetRefusal": flag}
			ct, body := eval(ctExpr, in), eval(bodyExpr, in)
			if !want {
				if body != nil || ct != c.headers["content-type"] && !(ct == nil && c.headers["content-type"] == nil) {
					t.Fatalf("%s %s: changed: content-type %v, body %v", c.name, path, ct, body)
				}
				continue
			}
			if ct != "application/json" {
				t.Fatalf("%s %s: content-type %v", c.name, path, ct)
			}
			b, _ := json.Marshal(body)
			s := string(b)
			if !strings.Contains(s, "Budget") || !strings.Contains(s, "request limit") || !strings.Contains(s, "spending limit") {
				t.Fatalf("%s %s: message does not say what was reached: %s", c.name, path, s)
			}
			anthropic := strings.Contains(s, `"type":"error"`) && strings.Contains(s, `"type":"rate_limit_error"`)
			openai := strings.Contains(s, `"code":"budget_exceeded"`) && strings.Contains(s, `"type":"insufficient_quota"`)
			if (path == "/v1/messages") != anthropic || anthropic == openai {
				t.Fatalf("%s %s: not in the API's error format: %s", c.name, path, s)
			}
		}
	}
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// budgetResponse is the response side of the Gateway's PreRouting transformation (empty when there is none).
func budgetResponse(t *testing.T, docs []doc) map[string]any {
	t.Helper()
	p := find(t, docs, "AgentgatewayPolicy", "ambit-llm-route-headers")
	r, _ := get(t, p, "spec", "traffic", "transformation").(map[string]any)["response"].(map[string]any)
	return r
}
