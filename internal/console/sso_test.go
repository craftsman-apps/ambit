package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

// fakeIdP serves an OpenID Connect discovery document; claimIssuer lets a test make it lie.
func fakeIdP(t *testing.T, claimIssuer func(self string) string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/acme/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		self := srv.URL + "/realms/acme"
		json.NewEncoder(w).Encode(map[string]any{"issuer": claimIssuer(self), "authorization_endpoint": self + "/auth",
			"token_endpoint": self + "/token", "scopes_supported": []string{"openid", "profile", "email"}, "claims_supported": []string{"sub", "groups"}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ssoFixture(t *testing.T) (http.Handler, client.Client, *Auth) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	return NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS, PublicURL: "https://console.acme.test",
		Connectors: []Connector{{ID: "keycloak", Name: "Acme (Keycloak)"}}}), c, auth
}

func TestCheckIssuerFindsTheMistakesDexWouldHit(t *testing.T) {
	h, _, a := ssoFixture(t)
	admin := user(a, "priya@acme.test", RoleAdmin)
	good := fakeIdP(t, func(self string) string { return self })
	r := do(t, h, "POST", "/api/sso/check", admin, map[string]string{"issuer": good.URL + "/realms/acme"})
	if r.Code != 200 {
		t.Fatalf("good issuer: %d %s", r.Code, r.Body)
	}
	got := decodeAs[IssuerCheck](t, r.Body.String())
	if got.RedirectURI != "https://console.acme.test/dex/callback" || !got.GroupsClaim || !strings.HasSuffix(got.TokenEndpoint, "/token") {
		t.Fatalf("%+v", got)
	}
	liar := fakeIdP(t, func(self string) string { return self + "/" })
	for _, tc := range []struct{ issuer, want string }{
		{liar.URL + "/realms/acme", "a trailing slash counts"},
		{good.URL + "/realms/other", "no OpenID Connect discovery document"},
		{"http://127.0.0.1:1/realms/acme", "could not reach"},
		{"login.acme.test", "must be a URL"},
	} {
		r := do(t, h, "POST", "/api/sso/check", admin, map[string]string{"issuer": tc.issuer})
		if r.Code != 400 || !strings.Contains(r.Body.String(), tc.want) {
			t.Errorf("%s: %d %s", tc.issuer, r.Code, r.Body)
		}
	}
	if r := do(t, h, "POST", "/api/sso/check", user(a, "alice@acme.test", RoleViewer), map[string]string{"issuer": good.URL}); r.Code != 403 {
		t.Fatalf("viewer: %d", r.Code)
	}
}

func TestConnectStoresTheSecretAndNeverReturnsIt(t *testing.T) {
	h, c, a := ssoFixture(t)
	admin := user(a, "priya@acme.test", RoleAdmin)
	idp := fakeIdP(t, func(self string) string { return self })
	const secret = "s3cr3t-client-value-9f2c"
	in := SSOInput{ID: "okta", Name: "Acme (Okta)", Issuer: idp.URL + "/realms/acme", ClientID: "ambit", ClientSecret: secret, Groups: true}
	r := do(t, h, "POST", "/api/sso/connectors", admin, in)
	if r.Code != http.StatusCreated {
		t.Fatalf("connect: %d %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), secret) {
		t.Fatal("the response contains the client secret")
	}
	res := decodeAs[SSOResult](t, r.Body.String())
	var s corev1.Secret
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "ambit-sso-okta"}, &s); err != nil {
		t.Fatal(err)
	}
	if s.StringData["clientSecret"] != secret && string(s.Data["clientSecret"]) != secret {
		t.Fatalf("secret data %v", s.StringData)
	}
	// The values must be exactly what the chart expects: a connector reading the secret from Dex's env.
	var v struct {
		SSO struct {
			Connectors []struct {
				Type, ID, Name string
				Config         map[string]any
			}
		}
		Dex struct {
			EnvVars []struct {
				Name      string
				ValueFrom struct {
					SecretKeyRef struct{ Name, Key string } `json:"secretKeyRef"`
				} `json:"valueFrom"`
			} `json:"envVars"`
		}
	}
	if err := yaml.Unmarshal([]byte(res.Values), &v); err != nil {
		t.Fatal(err)
	}
	cn := v.SSO.Connectors[0]
	// The fake provider lists no "groups" scope (like Keycloak), so none may be requested.
	if scopes, _ := cn.Config["scopes"].([]any); len(scopes) != 2 {
		t.Fatalf("scopes %v: a groups scope the provider lacks would break sign-in", cn.Config["scopes"])
	}
	if cn.Type != "oidc" || cn.ID != "okta" || cn.Config["clientSecret"] != "$SSO_OKTA_CLIENT_SECRET" ||
		cn.Config["redirectURI"] != "https://console.acme.test/dex/callback" || cn.Config["insecureEnableGroups"] != true {
		t.Fatalf("connector %+v", cn)
	}
	ev := v.Dex.EnvVars[0]
	if ev.Name != "SSO_OKTA_CLIENT_SECRET" || ev.ValueFrom.SecretKeyRef.Name != "ambit-sso-okta" || ev.ValueFrom.SecretKeyRef.Key != "clientSecret" {
		t.Fatalf("env %+v", ev)
	}
	if r := do(t, h, "POST", "/api/sso/connectors", admin, in); r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "kubectl -n ambit-system delete secret ambit-sso-okta") {
		t.Fatalf("second save: %d %s", r.Code, r.Body)
	}
	in.ID = "keycloak"
	if r := do(t, h, "POST", "/api/sso/connectors", admin, in); r.Code != http.StatusConflict {
		t.Fatalf("ID in use: %d", r.Code)
	}
	in.ID = "local"
	if r := do(t, h, "POST", "/api/sso/connectors", admin, in); r.Code != http.StatusBadRequest {
		t.Fatalf("local: %d", r.Code)
	}
}
