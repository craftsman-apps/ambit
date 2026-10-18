package identity

import (
	"encoding/json"
	"fmt"
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
	if r := budgetResponse(t, render(t)); len(get(t, r, "metadata").(map[string]any)) != 1 || contains(r["remove"], "x-ratelimit-limit") {
		t.Fatal("without limits.enabled the gateway must only rewrite unknown-path refusals")
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
	if len(meta) != 2 {
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
			in["metadata"] = map[string]any{"ambitBudgetRefusal": flag, "ambitUnknownPath": false}
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

// #115: a refusal for a path the LLM host does not serve becomes a 404 that names the path; known paths keep their
// 401/403 and every other host and status is untouched. Parse-only CEL, like the Budget test.
func TestUnknownLLMPathAnswers404(t *testing.T) {
	r := budgetResponse(t, render(t, "--set", "llm.host=llm.example.com"))
	flagExpr := get(t, r, "metadata", "ambitUnknownPath").(string)
	var statusExpr string
	for _, h := range get(t, r, "set").([]any) {
		if get(t, h, "name") == ":status" {
			statusExpr = get(t, h, "value").(string)
		}
	}
	bodyExpr := get(t, r, "body").(string)
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("request", cel.DynType), cel.Variable("response", cel.DynType), cel.Variable("metadata", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	eval := func(expr string, in map[string]any) any {
		t.Helper()
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
			t.Fatalf("evaluation failed (agentgateway would answer 500): %v\n%s\ninput %v", err, expr, in)
		}
		if v.Type() == types.NullType {
			return nil
		}
		if n, err := v.ConvertToNative(reflect.TypeOf(map[string]any{})); err == nil {
			return n
		}
		return v.Value()
	}
	cases := []struct {
		name, host, path string
		code             int64
		want             bool
	}{
		{"doubled base URL, valid key", "llm.example.com:8443", "/v1/v1/messages", 403, true},
		{"doubled base URL, no key", "llm.example.com", "/v1/v1/messages", 401, true},
		{"count_tokens is not served", "llm.example.com", "/v1/messages/count_tokens", 403, true},
		{"root", "llm.example.com", "/", 403, true},
		{"messages, no key", "llm.example.com", "/v1/messages", 401, false},
		{"chat completions, model refused", "llm.example.com", "/v1/chat/completions", 403, false},
		{"responses", "llm.example.com", "/v1/responses", 403, false},
		{"embeddings", "llm.example.com", "/v1/embeddings", 403, false},
		{"models", "llm.example.com", "/v1/models", 401, false},
		{"unknown path, but a 404 from elsewhere", "llm.example.com", "/nope", 404, false},
		{"console host", "console.example.com", "/api/me", 401, false},
		{"MCP host", "mcp.example.com", "/mcp", 403, false},
	}
	for _, c := range cases {
		in := map[string]any{"request": map[string]any{"path": c.path, "host": c.host}, "response": map[string]any{"code": c.code, "headers": map[string]any{}}}
		flag := eval(flagExpr, in)
		if flag != c.want {
			t.Errorf("%s: flag %v, want %v", c.name, flag, c.want)
		}
		in["metadata"] = map[string]any{"ambitUnknownPath": flag, "ambitBudgetRefusal": false}
		status, body := eval(statusExpr, in), eval(bodyExpr, in)
		if !c.want {
			if status != nil || body != nil {
				t.Errorf("%s: changed: status %v, body %v", c.name, status, body)
			}
			continue
		}
		b, _ := json.Marshal(body)
		if fmt.Sprint(status) != "404" || !strings.Contains(string(b), `"type":"not_found"`) || !strings.Contains(string(b), "no such path on the Ambit LLM gateway: "+c.path) {
			t.Errorf("%s: status %v, body %s", c.name, status, b)
		}
	}
	// No metadata key (its expression failed) must not make the others fail: agentgateway would answer 503.
	empty := map[string]any{"request": map[string]any{"path": "/x"}, "response": map[string]any{"code": int64(403), "headers": map[string]any{}}, "metadata": map[string]any{"ambitBudgetRefusal": false}}
	if eval(statusExpr, empty) != nil || eval(bodyExpr, empty) != nil {
		t.Error("without the metadata key the response must be left alone")
	}
}
