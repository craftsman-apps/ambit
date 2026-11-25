package identity

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
)

// evalAgent evaluates expr in cel-go for one MCP call (not agentgateway's CEL; test/stepagents.sh covers that).
func evalAgent(t *testing.T, expr string, key, jwt map[string]any, target, tool string) any {
	t.Helper()
	m := cel.MapType(cel.StringType, cel.DynType)
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("jwt", m), cel.Variable("apiKey", m), cel.Variable("mcp", m), cel.Variable("request", m))
	if err != nil {
		t.Fatal(err)
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		t.Fatalf("%s: %v", expr, iss.Err())
	}
	p, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := p.Eval(map[string]any{"jwt": jwt, "apiKey": key,
		"mcp":     map[string]any{"tool": map[string]any{"target": target, "name": tool}},
		"request": map[string]any{"startTime": types.Timestamp{Time: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}}})
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return v.Value()
}

const hydra = "https://auth.example.com"

func TestAgentScopeKeys(t *testing.T) {
	for _, c := range []struct {
		name         string
		key          map[string]any
		target, tool string
		want         bool
	}{
		{"key of a person, no agent", map[string]any{"keyId": "k", "email": "a@x.com"}, "crm", "delete", true},
		{"whole server", map[string]any{"keyId": "k", "agent": "g", "agentScope": []any{"crm"}}, "crm", "delete", true},
		{"other server", map[string]any{"keyId": "k", "agent": "g", "agentScope": []any{"crm"}}, "wiki", "read", false},
		{"one tool", map[string]any{"keyId": "k", "agent": "g", "agentScope": []any{"crm/read"}}, "crm", "read", true},
		{"other tool of the server", map[string]any{"keyId": "k", "agent": "g", "agentScope": []any{"crm/read"}}, "crm", "delete", false},
		{"a tool name is not a server", map[string]any{"keyId": "k", "agent": "g", "agentScope": []any{"read"}}, "crm", "read", false},
		{"server name equal to a tool entry of another server", map[string]any{"keyId": "k", "agent": "g", "agentScope": []any{"wiki/crm"}}, "crm", "read", false},
		{"empty scope: nothing", map[string]any{"keyId": "k", "agent": "g", "agentScope": []any{}}, "crm", "read", false},
		{"a map as scope matches nothing, although `in` finds its keys", map[string]any{"keyId": "k", "agent": "g", "agentScope": map[string]any{"crm": true}}, "crm", "read", false},
		{"a string as scope matches nothing", map[string]any{"keyId": "k", "agent": "g", "agentScope": "crm"}, "crm", "read", false},
		{"agent key without a scope: nothing (fail closed)", map[string]any{"keyId": "k", "agent": "g"}, "crm", "read", false},
		{"50 entries, the last one", map[string]any{"keyId": "k", "agent": "g", "agentScope": func() []any {
			l := make([]any, 50)
			for i := range l {
				l[i] = "server" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "/tool"
			}
			l[49] = "crm/read"
			return l
		}()}, "crm", "read", true},
	} {
		for _, issuer := range []string{"", hydra} {
			if got := evalAgent(t, AgentScope(issuer), c.key, map[string]any{}, c.target, c.tool); got != c.want {
				t.Errorf("%s (issuer %q): %v, want %v", c.name, issuer, got, c.want)
			}
		}
	}
}

// The jwt.* form: only a token whose validated issuer is the agent issuer is limited, and it must carry a scope.
func TestAgentScopeTokens(t *testing.T) {
	for _, c := range []struct {
		name         string
		issuer       string
		jwt          map[string]any
		target, tool string
		want         bool
	}{
		{"person token of the console's IdP", hydra, map[string]any{"iss": "https://dex.example.com", "email": "a@x.com"}, "crm", "read", true},
		{"agent token, whole server", hydra, map[string]any{"iss": hydra, "ambit_scope": []any{"crm"}}, "crm", "read", true},
		{"agent token, one tool", hydra, map[string]any{"iss": hydra, "ambit_scope": []any{"crm/read"}}, "crm", "read", true},
		{"agent token, other tool", hydra, map[string]any{"iss": hydra, "ambit_scope": []any{"crm/read"}}, "crm", "delete", false},
		{"agent token, empty scope", hydra, map[string]any{"iss": hydra, "ambit_scope": []any{}}, "crm", "read", false},
		{"agent token without a scope claim: nothing", hydra, map[string]any{"iss": hydra}, "crm", "read", false},
		{"agent token with a map as scope: nothing", hydra, map[string]any{"iss": hydra, "ambit_scope": map[string]any{"crm": true}}, "crm", "read", false},
		{"no agent issuer set: tokens are not limited", "", map[string]any{"iss": hydra, "ambit_scope": []any{"wiki"}}, "crm", "read", true},
	} {
		if got := evalAgent(t, AgentScope(c.issuer), map[string]any{}, c.jwt, c.target, c.tool); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAgentAttr(t *testing.T) {
	future := "2027-01-01T00:00:00Z"
	past := "2025-01-01T00:00:00Z"
	for _, c := range []struct {
		name             string
		field, claim, is string
		key, jwt         map[string]any
		want             string
	}{
		{"valid key", "agent", "ambit_agent", hydra, map[string]any{"keyId": "k", "until": future, "agent": "grok-y"}, map[string]any{}, "grok-y"},
		{"valid key of a person", "agent", "ambit_agent", hydra, map[string]any{"keyId": "k", "until": future}, map[string]any{}, ""},
		{"expired key", "agent", "ambit_agent", hydra, map[string]any{"keyId": "k", "until": past, "agent": "grok-y"}, map[string]any{}, ""},
		{"token of the agent issuer", "agent", "ambit_agent", hydra, map[string]any{}, map[string]any{"iss": hydra, "ambit_agent": "chatgpt-y"}, "chatgpt-y"},
		{"token of another issuer cannot name an agent", "agent", "ambit_agent", hydra, map[string]any{}, map[string]any{"iss": "https://evil.example.com", "ambit_agent": "chatgpt-y"}, ""},
		{"no agent issuer: claims are not read", "agent", "ambit_agent", "", map[string]any{}, map[string]any{"iss": hydra, "ambit_agent": "chatgpt-y"}, ""},
		{"grant id of a token", "", "ambit_grant", hydra, map[string]any{}, map[string]any{"iss": hydra, "ambit_grant": "g1"}, "g1"},
		{"grant id of a key", "", "ambit_grant", hydra, map[string]any{"keyId": "k", "until": future, "agent": "a"}, map[string]any{}, ""},
	} {
		if got := evalAgent(t, AgentAttr(c.field, c.claim, c.is), c.key, c.jwt, "", ""); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStopList(t *testing.T) {
	future := "2027-01-01T00:00:00Z"
	key := func(agent string) map[string]any {
		m := map[string]any{"keyId": "k", "until": future}
		if agent != "" {
			m["agent"] = agent
		}
		return m
	}
	for _, c := range []struct {
		name           string
		agents, grants []string
		issuer         string
		key, jwt       map[string]any
		want           any
	}{
		{"nothing listed", nil, nil, "", key("grok-y"), map[string]any{}, true},
		{"a listed agent's key", []string{"grok-y"}, nil, "", key("grok-y"), map[string]any{}, false},
		{"another agent's key", []string{"grok-y"}, nil, "", key("other"), map[string]any{}, true},
		{"a person's key (no agent)", []string{"grok-y"}, nil, "", key(""), map[string]any{}, true},
		{"a person's token", []string{"grok-y"}, nil, hydra, map[string]any{}, map[string]any{"iss": "https://dex.example.com", "email": "a@x.com"}, true},
		{"a listed agent's token", []string{"chatgpt-y"}, nil, hydra, map[string]any{}, map[string]any{"iss": hydra, "ambit_agent": "chatgpt-y"}, false},
		{"a token of another issuer naming a listed agent", []string{"chatgpt-y"}, nil, hydra, map[string]any{}, map[string]any{"iss": "https://evil.example.com", "ambit_agent": "chatgpt-y"}, true},
		{"a listed grant", nil, []string{"g1", "g2"}, hydra, map[string]any{}, map[string]any{"iss": hydra, "ambit_agent": "chatgpt-y", "ambit_grant": "g2"}, false},
		{"another grant of the same agent", nil, []string{"g1"}, hydra, map[string]any{}, map[string]any{"iss": hydra, "ambit_agent": "chatgpt-y", "ambit_grant": "g3"}, true},
		{"agent and grant lists: the agent matches", []string{"a"}, []string{"g1"}, hydra, map[string]any{}, map[string]any{"iss": hydra, "ambit_agent": "a", "ambit_grant": "g9"}, false},
		{"no issuer: tokens are not read", []string{"a"}, []string{"g1"}, "", map[string]any{}, map[string]any{"iss": hydra, "ambit_agent": "a", "ambit_grant": "g1"}, true},
	} {
		if got := evalAgent(t, StopList(c.agents, c.grants, c.issuer), c.key, c.jwt, "", ""); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// a full chunk: 150 names of 63 characters stay under 12,000 characters
	names := make([]string, 150)
	for i := range names {
		names[i] = fmt.Sprintf("%063d", i)
	}
	if n := len(StopList(names, names, hydra)); n > 25000 {
		t.Logf("a chunk with 150 names and 150 grants is %d characters", n)
	}
	if n := len(StopList(names, nil, "")); n > 12000 {
		t.Errorf("150 agent names: %d characters, the limit is 12,000", n)
	}
	if got := evalAgent(t, StopList(names, nil, ""), key(names[149]), map[string]any{}, "", ""); got != false {
		t.Errorf("the last of 150 names: %v", got)
	}
}
