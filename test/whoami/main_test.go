package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testIss = "http://keycloak.localtest.me:8080/realms/saas"
	testAud = "http://whoami.ambit-system.svc:8080/mcp"
)

func sign(t *testing.T, key *rsa.PrivateKey, c map[string]any) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(s).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestVerify(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	keys := &jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}
	now := time.Now()
	good := func() map[string]any {
		return map[string]any{"iss": testIss, "aud": []string{"account", testAud}, "exp": now.Add(time.Hour).Unix(), "preferred_username": "alice"}
	}
	mod := func(f func(map[string]any)) map[string]any { c := good(); f(c); return c }
	cases := []struct {
		name  string
		token string
		ok    bool
	}{
		{"valid, aud list", sign(t, key, good()), true},
		{"valid, aud string", sign(t, key, mod(func(c map[string]any) { c["aud"] = testAud })), true},
		{"wrong audience", sign(t, key, mod(func(c map[string]any) { c["aud"] = "http://other.test/mcp" })), false},
		{"no audience", sign(t, key, mod(func(c map[string]any) { delete(c, "aud") })), false},
		{"expired", sign(t, key, mod(func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() })), false},
		{"wrong issuer", sign(t, key, mod(func(c map[string]any) { c["iss"] = "http://evil.test" })), false},
		{"bad signature", sign(t, other, good()), false},
		{"not a token", "abc", false},
	}
	for _, c := range cases {
		raw, err := verify(c.token, keys, testIss, testAud, now)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
		if c.ok {
			var got struct {
				User string `json:"preferred_username"`
			}
			_ = json.Unmarshal(raw, &got)
			if got.User != "alice" {
				t.Errorf("%s: user %q", c.name, got.User)
			}
		}
	}
}
