package identity

import (
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
)

func TestCanonicalEmail(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"user01@acme.com", "user01@acme.com", true},
		{"  User01@Acme.COM ", "user01@acme.com", true},
		{"ÄUSER01@acme.com", "Äuser01@acme.com", true}, // ASCII-only folding, like CEL lowerAscii()
		{"", "", false},
		{"user01", "", false},
		{"a@b@c", "", false},
		{"al ice@acme.com", "", false},
		{"user01@", "", false},
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
	svc := `jwt.email.trim().lowerAscii().endsWith("@svc.ambit.internal")`
	if JWTGate(true) != `has(jwt.sub) && `+HasEmail()+` && !`+svc {
		t.Error(JWTGate(true))
	}
	if JWTGate(false) != `has(jwt.sub) && !jwt.sub.trim().lowerAscii().endsWith("@svc.ambit.internal") && !(`+HasEmail()+` && `+svc+`)` {
		t.Error(JWTGate(false))
	}
	for _, re := range []bool{true, false} {
		if Gate(re, true) != `(`+PresentedKey+`) ? (has(apiKey.keyId) && timestamp(apiKey.until) > request.startTime) : (`+JWTGate(re)+`)` {
			t.Error(Gate(re, true))
		}
		if Gate(re, false) != `(`+PresentedKey+`) ? (has(apiKey.keyId) && timestamp(apiKey.until) > request.startTime && !apiKey.email.endsWith("@svc.ambit.internal")) : (`+JWTGate(re)+`)` {
			t.Error(Gate(re, false))
		}
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
	for _, in := range []string{"user01@acme.com", "  User01@Acme.COM ", "ÄUSER01@acme.com", "o'brien@acme.com", "", "user01", "a@b@c", "al ice@acme.com", "user01@", "@acme.com"} {
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
		{true, map[string]any{"sub": "u1", "email": " User01@Acme.com ", "email_verified": true}, map[string]any{}, "user01@acme.com"},
		{true, map[string]any{"sub": "u1", "email": "user01@acme.com", "email_verified": false}, map[string]any{}, ""},
		{true, map[string]any{"sub": "u1", "email": "user01@acme.com", "email_verified": "true"}, map[string]any{}, ""},
		{true, map[string]any{"sub": "u1", "email": "user01@acme.com"}, map[string]any{}, ""},
		{false, map[string]any{"sub": "u1", "email": "user01@acme.com"}, map[string]any{}, "user01@acme.com"},
		{false, map[string]any{"sub": "u1", "email": " User01@Acme.com ", "email_verified": false}, map[string]any{}, "user01@acme.com"},
		{true, map[string]any{"sub": "user01@acme.com", "email_verified": true}, map[string]any{}, ""},
		{false, map[string]any{"sub": "user01@acme.com"}, map[string]any{}, ""},
		{false, map[string]any{"sub": "u1", "email": "not-an-email"}, map[string]any{}, ""},
		{true, map[string]any{}, map[string]any{"keyId": "k1", "email": "user02@acme.com"}, "user02@acme.com"},
		{false, map[string]any{}, map[string]any{"keyId": "k1", "email": "user02@acme.com"}, "user02@acme.com"},
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

// celEval evaluates expr in cel-go for one request (not agentgateway's CEL; the acceptance test covers that).
func celEval(t *testing.T, expr string, jwt, key map[string]any, headers map[string]any) (any, error) {
	t.Helper()
	env, err := cel.NewEnv(ext.Strings(), cel.Variable("jwt", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("apiKey", cel.MapType(cel.StringType, cel.DynType)), cel.Variable("request", cel.MapType(cel.StringType, cel.DynType)))
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
		"request": map[string]any{"headers": headers, "startTime": types.Timestamp{Time: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}}})
	if err != nil {
		return nil, err
	}
	return v.Value(), nil
}

// Guard 1: a JWT carrying the service account domain (any case, any spacing) is refused by the gate in every
// combination; with requireEmail false, a JWT without an email whose subject is in the domain is refused too. A
// normal JWT is accepted. serviceAccounts.enabled false refuses a service account's key and nothing else.
func TestGateRefusesTheServiceDomain(t *testing.T) {
	future := "2027-01-01T00:00:00Z"
	bearer := map[string]any{"authorization": "Bearer ambit_x"}
	for _, c := range []struct {
		name         string
		requireEmail bool
		services     bool
		jwt, key     map[string]any
		headers      map[string]any
		want         bool
	}{
		{"person JWT", true, true, map[string]any{"sub": "u1", "email": "user01@acme.com"}, map[string]any{}, map[string]any{}, true},
		{"reserved email", true, true, map[string]any{"sub": "u1", "email": "ci@svc.ambit.internal"}, map[string]any{}, map[string]any{}, false},
		{"reserved email, case and spaces", true, true, map[string]any{"sub": "u1", "email": " CI@SVC.Ambit.Internal "}, map[string]any{}, map[string]any{}, false},
		{"reserved email, requireEmail false", false, true, map[string]any{"sub": "u1", "email": " CI@SVC.Ambit.Internal "}, map[string]any{}, map[string]any{}, false},
		{"reserved subject, no email, requireEmail false", false, true, map[string]any{"sub": "CI@svc.ambit.internal"}, map[string]any{}, map[string]any{}, false},
		{"plain subject, no email, requireEmail false", false, true, map[string]any{"sub": "u1"}, map[string]any{}, map[string]any{}, true},
		{"person JWT, requireEmail false", false, true, map[string]any{"sub": "u1", "email": "user01@acme.com"}, map[string]any{}, map[string]any{}, true},
		{"service account key", true, true, map[string]any{}, map[string]any{"keyId": "k", "email": "ci@svc.ambit.internal", "until": future}, bearer, true},
		{"service account key, accounts off", true, false, map[string]any{}, map[string]any{"keyId": "k", "email": "ci@svc.ambit.internal", "until": future}, bearer, false},
		{"personal key, accounts off", true, false, map[string]any{}, map[string]any{"keyId": "k", "email": "user01@acme.com", "until": future}, bearer, true},
	} {
		got, err := celEval(t, Gate(c.requireEmail, c.services), c.jwt, c.key, c.headers)
		if (got == true) != c.want {
			t.Errorf("%s: Gate = %v, %v; want %v", c.name, got, err, c.want)
		}
		if len(c.key) == 0 { // keys switched off: JWTGate alone
			got, err := celEval(t, JWTGate(c.requireEmail), c.jwt, c.key, c.headers)
			if (got == true) != c.want {
				t.Errorf("%s: JWTGate = %v, %v; want %v", c.name, got, err, c.want)
			}
		}
	}
}

// The access log's identity type: "service account" exactly for a valid key in the service account domain (the
// invariant: only service account keys carry it).
func TestKind(t *testing.T) {
	future, bearer := "2027-01-01T00:00:00Z", map[string]any{"authorization": "Bearer ambit_x"}
	for _, c := range []struct {
		key     map[string]any
		headers map[string]any
		want    string
	}{
		{map[string]any{"keyId": "k", "email": "ci-deploy@svc.ambit.internal", "until": future}, bearer, "service account"},
		{map[string]any{"keyId": "k", "email": "user01@acme.com", "until": future}, bearer, "key"},
		{map[string]any{}, bearer, "invalid key"},
		{map[string]any{}, map[string]any{}, "jwt"},
	} {
		if got, err := celEval(t, Kind, map[string]any{"sub": "u1"}, c.key, c.headers); got != c.want {
			t.Errorf("Kind(%v) = %v, %v; want %s", c.key, got, err, c.want)
		}
	}
	for _, e := range []string{"ci@svc.ambit.internal", " CI@SVC.ambit.internal "} {
		if !IsService(e) {
			t.Errorf("IsService(%q) = false", e)
		}
	}
	for _, e := range []string{"ci@svc.ambit.internal.evil.com", "ci@example.com", "svc.ambit.internal", ""} {
		if IsService(e) {
			t.Errorf("IsService(%q) = true", e)
		}
	}
}
