package identity

import "testing"

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
