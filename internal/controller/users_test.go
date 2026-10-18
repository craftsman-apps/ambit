package controller

import (
	"slices"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/identity"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func userRule(name string, groups, users []string, models ...string) v1.AccessRule {
	return v1.AccessRule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ambit-system"},
		Spec: v1.AccessRuleSpec{Groups: groups, Users: users, Models: models}}
}

func TestCELWithUsers(t *testing.T) {
	g, e := identity.PrincipalGroups, identity.PrincipalEmail(true) // UsersRequireVerifiedEmail defaults to true
	for name, c := range map[string]struct {
		a    Access
		want string
	}{
		"users only":     {Access{Users: []string{"alice@acme.com", "bob@acme.com"}}, e + ` in ["alice@acme.com", "bob@acme.com"]`},
		"groups + users": {Access{Groups: []string{"eng"}, Users: []string{"alice@acme.com"}}, `(` + g + `.exists(g, g in ["eng"]) || ` + e + ` in ["alice@acme.com"])`},
		"allSignedIn":    {Access{AllSignedIn: true, Users: []string{"alice@acme.com"}}, "true"},
		"nobody":         {Access{}, "false"},
	} {
		if got := CEL(c.a); got != c.want {
			t.Errorf("%s: got %s, want %s", name, got, c.want)
		}
	}
}

// eval runs a generated expression in cel-go (not agentgateway's CEL; the e2e test covers that) for one caller.
func eval(t *testing.T, expr string, jwt, key map[string]any, tool string) bool {
	t.Helper()
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("jwt", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("apiKey", cel.MapType(cel.StringType, cel.DynType)), cel.Variable("mcp", cel.DynType))
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
		"mcp": map[string]any{"tool": map[string]any{"target": "crm", "name": tool}}})
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return v.Value() == true
}

var (
	alice      = map[string]any{"sub": "u-alice", "email": "Alice@Acme.com", "email_verified": true, "groups": []any{"sales"}}
	bob        = map[string]any{"sub": "u-bob", "email": "bob@acme.com", "email_verified": true, "groups": []any{"sales"}}
	emailless  = map[string]any{"sub": "alice@acme.com", "groups": []any{"sales"}} // requireEmail: false; a subject that looks like an email
	noKey      = map[string]any{}
	aliceKey   = map[string]any{"keyId": "k1", "email": "alice@acme.com", "groups": []any{}}
	pilotUsers = []string{"alice@acme.com"}
)

// A users-only rule lets the listed person in by JWT email (any case) or by personal key, and nobody else; an
// emailless token never matches, even when its subject equals a listed email.
func TestUsersRuleDecisions(t *testing.T) {
	res := Compile([]v1.Model{model("gemma", "google/gemma")}, nil, []v1.AccessRule{userRule("pilot", nil, pilotUsers, "gemma")}, route)
	x := expr(t, byName(res.Objects)["gemma"])
	for name, c := range map[string]struct {
		jwt, key map[string]any
		want     bool
	}{
		"alice by JWT":   {alice, noKey, true},
		"alice by key":   {map[string]any{}, aliceKey, true},
		"bob":            {bob, noKey, false},
		"emailless JWT":  {emailless, noKey, false},
		"key of someone": {alice, map[string]any{"keyId": "k2", "email": "bob@acme.com", "groups": []any{}}, false},
	} {
		if got := eval(t, x, c.jwt, c.key, ""); got != c.want {
			t.Errorf("%s: %v, want %v (%s)", name, got, c.want, x)
		}
	}
	if a := res.Access["gemma"]; len(a.Users) != 1 || a.Users[0] != "alice@acme.com" || len(a.Groups) != 0 {
		t.Fatalf("access %+v", a)
	}
}

// A JWT email counts for users only when verified (accessRules.usersRequireVerifiedEmail, default true); with the
// value false any well-formed email counts. A personal key matches by its owner's email either way.
func TestUsersRequireVerifiedEmail(t *testing.T) {
	unverified := map[string]any{"sub": "u-alice", "email": "alice@acme.com", "email_verified": false}
	noClaim := map[string]any{"sub": "u-alice", "email": "alice@acme.com"}
	for _, c := range []struct {
		verified bool
		jwt, key map[string]any
		want     bool
	}{
		{true, alice, noKey, true},
		{true, unverified, noKey, false},
		{true, noClaim, noKey, false},
		{true, map[string]any{}, aliceKey, true},
		{false, noClaim, noKey, true},
		{false, map[string]any{}, aliceKey, true},
		{false, emailless, noKey, false},
	} {
		UsersRequireVerifiedEmail = c.verified
		res := Compile([]v1.Model{model("gemma", "google/gemma")}, nil, []v1.AccessRule{userRule("pilot", nil, pilotUsers, "gemma")}, route)
		x := expr(t, byName(res.Objects)["gemma"])
		if got := eval(t, x, c.jwt, c.key, ""); got != c.want {
			t.Errorf("verified=%v jwt %v key %v: %v, want %v (%s)", c.verified, c.jwt, c.key, got, c.want, x)
		}
	}
	UsersRequireVerifiedEmail = true
}

// Rules merge users per model, sorted and without duplicates; a rule without users changes nothing (upgrade).
func TestUsersMergeAndUpgrade(t *testing.T) {
	res := Compile([]v1.Model{model("gemma", "google/gemma")}, nil, []v1.AccessRule{
		userRule("a", nil, []string{"bob@acme.com", "alice@acme.com"}, "gemma"),
		userRule("b", []string{"eng"}, []string{"alice@acme.com"}, "gemma"),
	}, route)
	if a := res.Access["gemma"]; len(a.Users) != 2 || a.Users[0] != "alice@acme.com" || a.Groups[0] != "eng" {
		t.Fatalf("access %+v", a)
	}
	old := Compile([]v1.Model{model("gemma", "google/gemma")}, nil, []v1.AccessRule{rule("r", []string{"eng"}, false, "gemma")}, route)
	if got := expr(t, old.Objects[0]); got != identity.PrincipalGroups+`.exists(g, g in ["eng"])` {
		t.Fatalf("a rule without users compiles differently: %s", got)
	}
}

// The unpriced refusal is evaluated on the caller's groups, also for a caller who reaches the model by users.
func TestUnpricedRefusesListedUserInADollarGroup(t *testing.T) {
	m := model("raw", "raw/model")
	res := Compile([]v1.Model{m}, nil, []v1.AccessRule{userRule("pilot", nil, pilotUsers, "raw")}, route)
	RefuseUnpriced(&res, []v1.Model{m}, nil, []string{"sales"})
	x := expr(t, res.Objects[0])
	if eval(t, x, alice, noKey, "") {
		t.Errorf("alice is in sales (dollar budget) but reached an unpriced model: %s", x)
	}
	if !eval(t, x, map[string]any{"sub": "u", "email": "alice@acme.com", "email_verified": true}, noKey, "") {
		t.Errorf("alice without a dollar-capped group refused: %s", x)
	}
}

// MCP: a tool granted to a person appears only for that person; groups + users on one tool keep the target and
// tool checks around both (precedence).
func TestMCPUsersPerTool(t *testing.T) {
	crm := server("crm", "stdio")
	rules := []v1.AccessRule{
		{ObjectMeta: metav1.ObjectMeta{Name: "pilot"}, Spec: v1.AccessRuleSpec{Users: pilotUsers,
			Servers: []v1.ServerAccess{{Name: "crm", Tools: []string{"export"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "eng"}, Spec: v1.AccessRuleSpec{Groups: []string{"eng"},
			Servers: []v1.ServerAccess{{Name: "crm", Tools: []string{"export"}}}}},
	}
	x := mcpExpr(t, Compile(nil, []v1.Server{crm}, rules, route))
	if !eval(t, x, alice, noKey, "export") || eval(t, x, bob, noKey, "export") || eval(t, x, emailless, noKey, "export") {
		t.Errorf("export: %s", x)
	}
	if eval(t, x, alice, noKey, "delete") {
		t.Errorf("a tool nobody was granted is allowed: %s", x)
	}
	a := ServerAccess([]v1.Server{crm}, nil, rules, "crm")
	if a.AllTools || len(a.Users) != 1 || a.Users[0] != "alice@acme.com" || len(a.ToolUsers["export"]) != 1 || a.Groups[0] != "eng" {
		t.Fatalf("effective access %+v", a)
	}
	whole := []v1.AccessRule{{Spec: v1.AccessRuleSpec{Users: pilotUsers, Servers: []v1.ServerAccess{{Name: "crm"}}}}}
	if x := mcpExpr(t, Compile(nil, []v1.Server{crm}, whole, route)); !eval(t, x, alice, noKey, "any") || eval(t, x, bob, noKey, "any") {
		t.Errorf("whole server for alice: %s", x)
	}
	if a := ServerAccess([]v1.Server{crm}, nil, whole, "crm"); !a.AllTools {
		t.Fatalf("users-only whole-server grant must count as all tools: %+v", a)
	}
}

func TestMayShareWithUsers(t *testing.T) {
	for name, c := range map[string]struct {
		a, b    Access
		refused []string
		want    bool
	}{
		"same user":                  {Access{Users: pilotUsers}, Access{Users: pilotUsers}, nil, true},
		"same user, refused groups":  {Access{Users: pilotUsers}, Access{Users: pilotUsers}, []string{"sales"}, true},
		"different users":            {Access{Users: pilotUsers}, Access{Users: []string{"bob@acme.com"}}, nil, false},
		"user and group":             {Access{Users: pilotUsers}, Access{Groups: []string{"eng"}}, nil, false},
		"router open, default users": {Access{AllSignedIn: true}, Access{Users: pilotUsers}, nil, true},
		"router users, default open": {Access{Users: pilotUsers}, Access{AllSignedIn: true}, nil, true},
		"default nobody":             {Access{Users: pilotUsers}, Access{}, nil, false},
	} {
		if got := mayShare(c.a, c.b, c.refused); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// A router granted to a person sends that person's users to ambit-router's configuration.
func TestRouterConfigCarriesUsers(t *testing.T) {
	rules := []v1.AccessRule{userRule("pilot", nil, pilotUsers, "auto"), userRule("all", nil, pilotUsers, "gemma", "sonnet")}
	_, conds, cfg := compileLLM(append(slices.Clone(catalog), local), rules, installed, llmRouter("auto", "coding"))
	if len(cfg.Routers) != 1 || len(cfg.Routers[0].Users) != 1 || cfg.Routers[0].Users[0] != "alice@acme.com" {
		t.Fatalf("config %+v", cfg.Routers)
	}
	if conds["auto"].Reason == "DefaultNotAllowed" {
		t.Fatalf("alice may use both: %+v", conds["auto"])
	}
}
