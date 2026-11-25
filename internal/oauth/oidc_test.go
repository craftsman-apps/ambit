package oauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// miniIdP is an OpenID provider over TLS for the tests of NewOIDC and Exchange.
type miniIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	claims map[string]any // the ID token the token endpoint returns
	plain  bool           // advertise an http token endpoint
	iss    bool
}

func newMiniIdP(t *testing.T) *miniIdP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &miniIdP{key: k}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		tok := m.srv.URL + "/token"
		if m.plain {
			tok = "http://" + strings.TrimPrefix(m.srv.URL, "https://") + "/token"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": m.srv.URL, "authorization_endpoint": m.srv.URL + "/authorize", "token_endpoint": tok,
			"jwks_uri": m.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}, "authorization_response_iss_parameter_supported": m.iss})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sig, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
		raw, _ := jwt.Signed(sig).Claims(m.claims).Serialize()
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "x", "token_type": "Bearer", "id_token": raw})
	})
	m.srv = httptest.NewTLSServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *miniIdP) cfg() Config {
	return Config{PublicURL: pubURL, IdPIssuer: m.srv.URL, IdPClientID: "ambit", IdPClientSecret: "s", GroupsClaim: "groups"}
}

func (m *miniIdP) token(extra map[string]any) {
	c := map[string]any{"iss": m.srv.URL, "sub": "u1", "aud": "ambit", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(),
		"nonce": "n", "email": "a@b.c", "email_verified": true, "groups": []string{"eng"}}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	m.claims = c
}

func TestOIDCEndpointsMustBeHTTPS(t *testing.T) {
	m := newMiniIdP(t)
	m.plain = true
	if _, err := NewOIDCClient(context.Background(), m.cfg(), m.srv.Client()); err == nil || !strings.Contains(err.Error(), "token_endpoint is not an https address") {
		t.Fatalf("a token endpoint over http must be refused: %v", err)
	}
	m.plain = false
	o, err := NewOIDCClient(context.Background(), m.cfg(), m.srv.Client())
	if err != nil || o.IssRequired() {
		t.Fatalf("%v", err)
	}
	m.iss = true
	if o, _ = NewOIDCClient(context.Background(), m.cfg(), m.srv.Client()); !o.IssRequired() {
		t.Fatal("the provider says it sends iss: it is required")
	}
}

func TestOIDCAudienceAndAuthorizedParty(t *testing.T) {
	m := newMiniIdP(t)
	o, err := NewOIDCClient(context.Background(), m.cfg(), m.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		claims map[string]any
		ok     bool
	}{
		"one audience":                   {map[string]any{}, true},
		"several audiences with azp":     {map[string]any{"aud": []string{"ambit", "other"}, "azp": "ambit"}, true},
		"several audiences without azp":  {map[string]any{"aud": []string{"ambit", "other"}}, false},
		"azp is another client":          {map[string]any{"azp": "other"}, false},
		"several audiences, azp another": {map[string]any{"aud": []string{"ambit", "other"}, "azp": "other"}, false},
		"not our audience":               {map[string]any{"aud": "other"}, false},
		"expired":                        {map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, false},
		"another issuer":                 {map[string]any{"iss": "https://evil.example.com"}, false},
	} {
		m.token(tc.claims)
		c, err := o.Exchange(context.Background(), "code", "verifier-verifier-verifier-verifier-1234")
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
		if err == nil && (c.Email != "a@b.c" || !c.EmailVerified || len(c.Groups) != 1 || c.Nonce != "n") {
			t.Errorf("%s: claims %+v", name, c)
		}
	}
}
