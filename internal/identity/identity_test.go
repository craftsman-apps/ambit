package identity

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

func TestCanonicalEmail(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"alice@acme.com", "alice@acme.com", true},
		{"  Alice@Acme.COM ", "alice@acme.com", true},
		{"ÄLICE@acme.com", "Älice@acme.com", true}, // ASCII-only folding, like CEL lowerAscii()
		{"", "", false},
		{"alice", "", false},
		{"a@b@c", "", false},
		{"al ice@acme.com", "", false},
		{"alice@", "", false},
		{"@acme.com", "", false},
	} {
		got, ok := CanonicalEmail(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("CanonicalEmail(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFragmentsArePinned(t *testing.T) {
	if Principal(true) != `(has(apiKey.keyId) ? apiKey.email : jwt.email.trim().lowerAscii())` {
		t.Error(Principal(true))
	}
	if Principal(false) != `(has(apiKey.keyId) ? apiKey.email : ((`+HasEmail()+`) ? jwt.email.trim().lowerAscii() : jwt.sub))` {
		t.Error(Principal(false))
	}
	if PrincipalGroups != `(has(apiKey.keyId) ? apiKey.groups : (has(jwt.groups) ? jwt.groups : []))` {
		t.Error(PrincipalGroups)
	}
	if Gate(true) != `(`+PresentedKey+`) ? (has(apiKey.keyId) && timestamp(apiKey.until) > request.startTime) : (has(jwt.sub) && `+HasEmail()+`)` {
		t.Error(Gate(true))
	}
	if Gate(false) != `(`+PresentedKey+`) ? (has(apiKey.keyId) && timestamp(apiKey.until) > request.startTime) : has(jwt.sub)` {
		t.Error(Gate(false))
	}
	if JWTGate(true) != `has(jwt.sub) && `+HasEmail() || JWTGate(false) != `has(jwt.sub)` {
		t.Error(JWTGate(true), JWTGate(false))
	}
}

// The CEL email rule and the Go rule must agree (cel-go is not agentgateway's CEL; the acceptance test covers that).
func TestEmailRuleParity(t *testing.T) {
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("jwt", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		t.Fatal(err)
	}
	prog := func(expr string) cel.Program {
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			t.Fatal(iss.Err())
		}
		p, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	has, canon := prog(HasEmail()), prog(`jwt.email.trim().lowerAscii()`)
	for _, in := range []string{"alice@acme.com", "  Alice@Acme.COM ", "ÄLICE@acme.com", "o'brien@acme.com", "", "alice", "a@b@c", "al ice@acme.com", "alice@", "@acme.com"} {
		want, wantOK := CanonicalEmail(in)
		act := map[string]any{"jwt": map[string]any{"email": in}}
		v, _, err := has.Eval(act)
		if err != nil || v.Value() != wantOK {
			t.Errorf("HasEmail(%q) = %v, %v; want %v", in, v, err, wantOK)
			continue
		}
		if wantOK {
			if c, _, err := canon.Eval(act); err != nil || c.Value() != want {
				t.Errorf("canonical(%q) = %v, %v; want %q", in, c, err, want)
			}
		}
	}
	// Not a string, or missing: never usable, never an error.
	for _, jwt := range []map[string]any{{"email": 5}, {}} {
		if v, _, err := has.Eval(map[string]any{"jwt": jwt}); err != nil || v.Value() != false {
			t.Errorf("HasEmail(%v) = %v, %v; want false", jwt, v, err)
		}
	}
}

// PrincipalEmail never yields the subject: an emailless JWT gives "", which no AccessRule user (pattern-checked
// email) equals. A key gives its owner's email. With requireVerified, a JWT's email counts only with
// email_verified: true.
func TestPrincipalEmail(t *testing.T) {
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("jwt", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("apiKey", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		verified bool
		jwt, key map[string]any
		want     string
	}{
		{true, map[string]any{"sub": "u1", "email": " Alice@Acme.com ", "email_verified": true}, map[string]any{}, "alice@acme.com"},
		{true, map[string]any{"sub": "u1", "email": "alice@acme.com", "email_verified": false}, map[string]any{}, ""},
		{true, map[string]any{"sub": "u1", "email": "alice@acme.com", "email_verified": "true"}, map[string]any{}, ""},
		{true, map[string]any{"sub": "u1", "email": "alice@acme.com"}, map[string]any{}, ""},
		{false, map[string]any{"sub": "u1", "email": "alice@acme.com"}, map[string]any{}, "alice@acme.com"},
		{false, map[string]any{"sub": "u1", "email": " Alice@Acme.com ", "email_verified": false}, map[string]any{}, "alice@acme.com"},
		{true, map[string]any{"sub": "alice@acme.com", "email_verified": true}, map[string]any{}, ""},
		{false, map[string]any{"sub": "alice@acme.com"}, map[string]any{}, ""},
		{false, map[string]any{"sub": "u1", "email": "not-an-email"}, map[string]any{}, ""},
		{true, map[string]any{}, map[string]any{"keyId": "k1", "email": "bob@acme.com"}, "bob@acme.com"},
		{false, map[string]any{}, map[string]any{"keyId": "k1", "email": "bob@acme.com"}, "bob@acme.com"},
	} {
		ast, iss := env.Compile(PrincipalEmail(c.verified))
		if iss.Err() != nil {
			t.Fatal(iss.Err())
		}
		p, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		v, _, err := p.Eval(map[string]any{"jwt": c.jwt, "apiKey": c.key})
		if err != nil || v.Value() != c.want {
			t.Errorf("PrincipalEmail(%v)(jwt %v, key %v) = %v, %v; want %q", c.verified, c.jwt, c.key, v, err, c.want)
		}
	}
}
